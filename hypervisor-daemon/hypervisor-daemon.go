package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/garyburd/redigo/redis"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"text/template"
	"time"
)

// A struct to hold the list of VMs
type VMList struct {
	mux sync.Mutex
	Vms map[int]VMInformation
}

type VMInformation struct {
	URL    string
	Status string
	Id     int
}

func (v *VMList) addVM(vmId int, status string, url string) error {
	// VMs < 50 are reserved for administrative use
	if vmId < 50 || vmId > 254 {
		return errors.New("invalid")
	}

	// Lock for our further checks
	v.mux.Lock()
	defer v.mux.Unlock()

	// Get the list of VMs and confirm whether this is one that's already running
	if _, exists := v.Vms[vmId]; exists {
		return errors.New("in use")
	}

	// It's both valid and not in use!
	vm := VMInformation{Id: vmId, Status: status, URL: url}
	v.Vms[vmId] = vm

	return nil // no errors
}

func (v *VMList) removeVM(vmId int) error {
	v.mux.Lock()
	defer v.mux.Unlock()

	// If the VM doesn't already exist in the map, return an error
	if _, ok := v.Vms[vmId]; !ok {
		return errors.New("Invalid vmId specified")
	}

	delete(v.Vms, vmId)

	return nil
}

func (v *VMList) updateVM(vmId int, status string, url string) error {
	v.mux.Lock()
	defer v.mux.Unlock()

	// If the VM doesn't already exist in the map, return an error
	if _, ok := v.Vms[vmId]; !ok {
		return errors.New("Invalid vmId specified")
	}

	VMInfo := VMInformation{URL: url, Status: status, Id: vmId}
	v.Vms[vmId] = VMInfo

	return nil
}

func (v *VMList) sync() error {
	// Shell out to get a list of screen sessions, which are VMs
	out, err := exec.Command("screen", "-ls").Output()
	if err != nil {
		return errors.New("Error running screen")
	}

	// Our new map
	newvms := make(map[int]VMInformation)

	// Regex out the running VM IDs
	re := regexp.MustCompile(`\b\.vm([0-9]+)\b`)

	matches := re.FindAllStringSubmatch(string(out), -1)

	// We only need to start locking now, to ensure we read a valid state
	v.mux.Lock()
	defer v.mux.Unlock()

	for i := 0; i < len(matches); i++ {
		// Ignore invalid VMs
		id, err := strconv.Atoi(matches[i][1])
		if err == nil {
			// Create a new struct for it
			// Only get the state of it already exists
			var vminfo VMInformation
			if val, ok := v.Vms[id]; ok {
				vminfo = val
			} else {
				vminfo = VMInformation{Id: id, Status: "running"}
			}
			// Look up the URL if required
			if newvms[id].URL == "" {
				resp, err := http.Get(fmt.Sprintf("http://10.0.0.5/view/%v", id))
				if err != nil {
					vminfo.Status = "broken"
					newvms[id] = vminfo
					fmt.Fprintln(os.Stderr, "error talking to torcontrol: %v", err)
					continue
				}
				body, err := ioutil.ReadAll(resp.Body)
				if err != nil {
					vminfo.Status = "broken"
					newvms[id] = vminfo
					fmt.Fprintln(os.Stderr, "error getting response from torcontrol: %v", err)
					continue
				}
				status := string(body)
				// Close the response body
				resp.Body.Close()
				if status == "invalid" || status == "unknown" {
					vminfo.Status = "broken"
					newvms[id] = vminfo
					continue
				}
				vminfo.URL = status
				vminfo.Status = "running"
				newvms[id] = vminfo
			}
		}
	}

	// Now replace the map
	v.Vms = newvms

	return nil // no error!
}

func main() {
	os.Exit(run())
}

func run() int {
	// Get our VM struct working
	v := VMList{Vms: make(map[int]VMInformation)}
	v.sync()

	// Connect here rather than in the handler so that we can ensure we can connect and exit if need be
	{
		// Scope our variable to ensure no one accidently uses the connection
		redisCon, err := redis.Dial("tcp", "10.0.5.20:6379")
		if err != nil {
			fmt.Printf("Could not connect to redis database: %v", err)
			return 1
		}
		go redisPubSubHandle(redisCon, v)
		defer redisCon.Close()

	}

	// Get the current numbers and data about VMs
	http.HandleFunc("/sync", func(w http.ResponseWriter, r *http.Request) {
		syncHandler(w, r, v)
	})

	// View information about a given VM
	http.HandleFunc("/view/", func(w http.ResponseWriter, r *http.Request) {
		viewHandler(w, r, v)
	})

	// Create a new VM of a given ID
	http.HandleFunc("/create/", func(w http.ResponseWriter, r *http.Request) {
		createHandler(w, r, v)
	})

	// Only bind to one interface -- IMPORTANT
	http.ListenAndServe("10.0.5.20:80", nil)

	return 0
}

func redisPubSubHandle(redisCon redis.Conn, vmlist VMList) {
	psc := redis.PubSubConn{Conn: redisCon}
	psc.Subscribe("deletevm")

	for {
		// TODO Can we use an if/continue instead of a switch?
		switch v := psc.Receive().(type) {
		case redis.Message:
			switch v.Channel {
			case "deletevm":
				// Parse out the ID and if required, do the deed
				vmId, err := strconv.Atoi(string(v.Data))
				// Check whether the ID is valid
				if err != nil {
					continue
				}
				if vmId < 50 || vmId > 255 {
					return
				}

				go deleteVm(vmId, vmlist)
			}
		}
		fmt.Println(fmt.Sprintf("[%v] Got a PUBSUB message", time.Now()))
	}
}

func deleteVm(vmId int, v VMList) {
	// Change state
	v.updateVM(vmId, "deleting", v.Vms[vmId].URL)

	// Remove the screen session
	cmd := exec.Command("screen", "-X", "-S", fmt.Sprintf("vm%v", vmId), "quit")
	out, err := cmd.Output()
	outStr := string(out)
	if err != nil {
		v.updateVM(vmId, "broken", "")
		fmt.Fprintln(os.Stderr, "error starting screen session for new VM: %v \r\nadditional: %v", err, outStr)
		return
	}

	// deactivate the bridge
	err = exec.Command(fmt.Sprintf("/etc/init.d/net.br%v", vmId), "stop").Run()
	if err != nil {
		v.updateVM(vmId, "broken", "")
		fmt.Fprintln(os.Stderr, "error executing bridge restart for new VM: %v", err)
		return
	}

	// Bring down the vlan
	out, err = exec.Command("ip", "link", "del", fmt.Sprintf("enp3s0.%v", vmId)).Output()
	if err != nil {
		v.updateVM(vmId, "broken", "")
		fmt.Fprintln(os.Stderr, fmt.Sprintf("error removing vlan: %v %s", err, out))
		return
	}

	// Remove autostart of bridge
	err = exec.Command("rc-update", "del", fmt.Sprintf("net.br%v", vmId)).Run()
	if err != nil {
		v.updateVM(vmId, "broken", "")
		fmt.Fprintln(os.Stderr, "error removing bridge from default runlevel: %v", err)
		return
	}

	// Remove the bridge symlink
	err = os.Remove(fmt.Sprintf("/etc/init.d/net.br%v", vmId))
	if err != nil {
		v.updateVM(vmId, "broken", "")
		fmt.Fprintf(os.Stderr, "error creating bridge symilnk for new VM: %v", err)
		return
	}

	// Remove the disk image
	err = os.RemoveAll(fmt.Sprintf("/root/vm-images/vm%v/", vmId))
	if err != nil {
		v.updateVM(vmId, "broken", "")
		fmt.Fprintf(os.Stderr, "error removing disk image VM: %v", err)
		return
	}

	// Complete deletion by removing it from the list of VMs
	v.removeVM(vmId)

	// We really should regenerate our net file here, but if we don't, it's not the end of the world (sucks to be the person to clean up after the reboot HA
	// TODO ^ I'm going to regret this later, fix it
}

func viewHandler(w http.ResponseWriter, r *http.Request, v VMList) {
	fmt.Println(fmt.Sprintf("[%v] %v", time.Now(), r.URL.Path))
	vmIdStr := r.URL.Path[len("/view/"):]
	vmId, err := strconv.Atoi(vmIdStr)
	// Check whether the ID is valid
	if err != nil {
		fmt.Fprintf(w, "invalid")
		return
	}

	// VMs < 50 are reserved for administrative use
	if vmId < 50 || vmId > 254 {
		fmt.Fprintf(w, "invalid")
		return
	}

	// Confirm there is a vm running
	if _, exists := v.Vms[vmId]; !exists {
		fmt.Fprintf(w, "invalid")
		return
	}

	// valid and running
	fmt.Fprintf(w, v.Vms[vmId].URL)
}

func syncHandler(w http.ResponseWriter, r *http.Request, v VMList) {
	v.sync()
	// build a new map of data that json can encode
	inter := make(map[string]VMInformation)
	for k, value := range v.Vms {
		inter[strconv.Itoa(k)] = value
	}
	// json encode our data and write it out
	b, err := json.Marshal(inter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error json encoding our VMs: %v", err)
		return
	}
	w.Write(b)
}

func createHandler(w http.ResponseWriter, r *http.Request, v VMList) {
	fmt.Println(fmt.Sprintf("[%v] %v", time.Now(), r.URL.Path))
	vmIdStr := r.URL.Path[len("/create/"):]
	vmId, err := strconv.Atoi(vmIdStr)
	// Check whether the ID is valid
	if err != nil {
		fmt.Fprintf(w, "invalid")
		return
	}

	err = v.addVM(vmId, "creating", "")
	if err != nil {
		fmt.Fprintf(w, fmt.Sprintf("%v", err))
		return
	}

	// No error means we're ready to start the VM
	// Fork off a new thread to do the creation then let the user know we've started
	go createVM(vmId, v)
	fmt.Fprintf(w, "creating")
}

func createVM(vmId int, v VMList) {
	// This function assumes it's already been put into VMInformation
	// TODO: Write a validator for above asumption ^

	// Create a new directory for the VM disk image
	err := os.Mkdir(fmt.Sprintf("/root/vm-images/vm%v", vmId), 0755)
	if err != nil {
		v.updateVM(vmId, "broken", "")
		fmt.Fprintf(os.Stderr, "error creating disk image directory for new VM: %v", err)
		return
	}

	// Create new disk image
	err = exec.Command("qemu-img", "create", "-f", "qcow2", "-o", "backing_file=/root/vm-images/base-gentoo-vanilla-v3.img", fmt.Sprintf("/root/vm-images/vm%v/vm%v-gentoo-vanilla-v3.img", vmId, vmId)).Run()
	if err != nil {
		v.updateVM(vmId, "broken", "")
		fmt.Fprintf(os.Stderr, "error creating disk image for new VM: %v", err)
		return
	}

	// Create our network bridge and configuration
	// TODO: Do we need to lock the datastructure here? We might write network information for a VM that isn't made yet
	t, err := template.ParseFiles("assets/net")
	if err != nil {
		v.updateVM(vmId, "broken", "")
		fmt.Fprintf(os.Stderr, "error creating template for new VM: %v", err)
		return
	}
	var net bytes.Buffer
	err = t.Execute(&net, v)
	if err != nil {
		v.updateVM(vmId, "broken", "")
		fmt.Fprintf(os.Stderr, "error executing template for new VM: %v", err)
		return
	}

	// Write the file
	err = ioutil.WriteFile("/etc/conf.d/net", net.Bytes(), 0644)
	if err != nil {
		v.updateVM(vmId, "broken", "")
		fmt.Fprintf(os.Stderr, "error writing net template for new VM: %v", err)
		return
	}

	// Create the symlink for the bridge
	err = os.Symlink("/etc/init.d/net.bridge", fmt.Sprintf("/etc/init.d/net.br%v", vmId))
	if err != nil {
		v.updateVM(vmId, "broken", "")
		fmt.Fprintf(os.Stderr, "error creating bridge symilnk for new VM: %v", err)
		return
	}

	// Manually bring up the new vlan
	out, err := exec.Command("ip", "link", "add", "link", "enp3s0", "name", fmt.Sprintf("enp3s0.%v", vmId), "type", "vlan", "id", fmt.Sprintf("%v", vmId)).Output()
	if err != nil {
		v.updateVM(vmId, "broken", "")
		fmt.Fprintln(os.Stderr, fmt.Sprintf("error adding new vlan: %v %s", err, out))
		return
	}

	// Activate the new bridge
	err = exec.Command(fmt.Sprintf("/etc/init.d/net.br%v", vmId), "start").Run()
	if err != nil {
		v.updateVM(vmId, "broken", "")
		fmt.Fprintln(os.Stderr, "error executing bridge restart for new VM: %v", err)
		return
	}

	// Configure the bridge to start on boot
	err = exec.Command("rc-update", "add", fmt.Sprintf("net.br%v", vmId), "default").Run()
	if err != nil {
		v.updateVM(vmId, "broken", "")
		fmt.Fprintln(os.Stderr, "error adding bridge to default runlevel: %v", err)
		return
	}

	// Create a new screen/qemu instance
	cmd := exec.Command("screen", "-d", "-m", "-S", fmt.Sprintf("vm%v", vmId), "qemu-system-x86_64", "-nographic", "-enable-kvm", "-cpu", "host", "-curses", "-m", "512M", "-drive", fmt.Sprintf("file=/root/vm-images/vm%v/vm%v-gentoo-vanilla-v3.img,if=virtio", vmId, vmId), "-netdev", fmt.Sprintf("tap,helper=/usr/libexec/qemu-bridge-helper --br=br%v,id=hn0", vmId), "-device", "virtio-net-pci,netdev=hn0,id=nic1", "-append", fmt.Sprintf("root=/dev/vda4 ro vmid=%v", vmId), "-kernel", "/root/vm-images/kernels/vmlinuz-4.7.10-hardened")
	// TODO: Find a way to check the error properly without using .Run() -- CHANGE TO .Start() else it won't work in futuer
	out, err = cmd.Output()
	outStr := string(out)
	if err != nil {
		v.updateVM(vmId, "broken", "")
		fmt.Fprintln(os.Stderr, "error starting screen session for new VM: %v \r\nadditional: %v", err, outStr)
		return
	}

	// Talk to the raspberry pi about getting a new Tor set up
	resp, err := http.Get(fmt.Sprintf("http://10.0.0.5/create/%v", vmId))
	if err != nil {
		v.updateVM(vmId, "broken", "")
		fmt.Fprintln(os.Stderr, "error talking to torcontrol: %v", err)
		return
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		v.updateVM(vmId, "broken", "")
		fmt.Fprintln(os.Stderr, "error getting response from torcontrol: %v", err)
		return
	}

	// Determine if the final part, tor stuff, worked
	status := string(body)

	// Close the response body
	resp.Body.Close()

	if status != "creating" {
		// TODO we should never get here, so handle this more strongly, it's probably an attack?
		fmt.Fprintln(os.Stderr, fmt.Sprintf("error talking to torcontrol, response then err: %v -- %v", status, err))
		v.updateVM(vmId, "broken", "")
		return
	}

	// Wait a while for tor to generate it
	time.Sleep(30 * time.Second)

	// fetch the hostname
	resp, err = http.Get(fmt.Sprintf("http://10.0.0.5/view/%v", vmId))
	if err != nil {
		v.updateVM(vmId, "broken", "")
		fmt.Fprintln(os.Stderr, "error talking to torcontrol: %v", err)
		return
	}
	body, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		v.updateVM(vmId, "broken", "")
		fmt.Fprintln(os.Stderr, "error getting response from torcontrol: %v", err)
		return
	}

	// Determine if the final part, tor stuff, worked
	status = string(body)

	// Close the response body
	resp.Body.Close()

	if status == "invalid" || status == "unknown" {
		// TODO we should never get here, so handle this more strongly, it's probably an attack?
		fmt.Fprintln(os.Stderr, fmt.Sprintf("error talking to torcontrol for hostname, response then status: %v -- %v", status, err))
		v.updateVM(vmId, "broken", "")
		return
	}

	// Update VM status to be the onion address
	v.updateVM(vmId, "complete", status)
}
