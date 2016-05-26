package main

import (
	"fmt"
	"os"
	"os/exec"
	"net/http"
	"strconv"
	"regexp"
	"errors"
	"sync"
	"text/template"
	"bytes"
	"io/ioutil"
)

type VMInformation struct {
	mux sync.Mutex
	// This map is id => state (should be an enum I guess)
	Vms map[int]string
}

func (v *VMInformation) addVM(vmId int, state string) (error) {
	// VMs < 50 are reserved for administrative use
	if (vmId < 50 || vmId > 254) {
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
	v.Vms[vmId] = state

	return nil // no errors
}

func (v *VMInformation) updateVM(vmId int, state string) (error) {
	v.mux.Lock()
	defer v.mux.Unlock()

	// If the VM doesn't already exist in the map, return an error
	if _, ok := v.Vms[vmId]; !ok {
		return errors.New("Invalid vmId specified")
	}

	v.Vms[vmId] = state

	return nil
}

func (v *VMInformation) sync() (error) {
	// Shell out to get a list of screen sessions, which are VMs
	out, err := exec.Command("screen", "-ls").Output()
	if err != nil {
		return errors.New("Error running screen")
	}

	// Our new map
	newvms := make(map[int]string)

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
			// Only get the state of it already exists
			if val, ok := v.Vms[id]; ok {
				newvms[id] = val
			} else {
				newvms[id] = "running"
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

func run() (int) {
	// Get our VM struct working
	v := VMInformation{Vms: make(map[int]string)}
	v.sync()

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

func viewHandler(w http.ResponseWriter, r *http.Request, v VMInformation) {
	vmIdStr := r.URL.Path[len("/view/"):]
	vmId, err := strconv.Atoi(vmIdStr)
	// Check whether the ID is valid
	if err != nil {
		fmt.Fprintf(w, "invalid")
		return
	}

	// VMs < 50 are reserved for administrative use
	if (vmId < 50 || vmId > 254) {
		fmt.Fprintf(w, "invalid")
		return
	}

	// Confirm there is a vm running
	if _, exists := v.Vms[vmId]; !exists {
		fmt.Fprintf(w, "invalid")
		return
	}

	// valid and running
	fmt.Fprintf(w, v.Vms[vmId])
}

func syncHandler(w http.ResponseWriter, r *http.Request, v VMInformation) {
	// TODO: implement
}

func createHandler(w http.ResponseWriter, r *http.Request, v VMInformation) {
	vmIdStr := r.URL.Path[len("/create/"):]
	vmId, err := strconv.Atoi(vmIdStr)
	// Check whether the ID is valid
	if err != nil {
		fmt.Fprintf(w, "invalid")
		return
	}

	err = v.addVM(vmId, "creating")
	if err != nil {
		fmt.Fprintf(w, fmt.Sprintf("%v", err))
		return
	}

	// No error means we're ready to start the VM
	// Fork off a new thread to do the creation then let the user know we've started
	go createVM(vmId, v)
	fmt.Fprintf(w, "creating")
}

func createVM(vmId int, v VMInformation) {
	// This function assumes it's already been put into VMInformation
	// TODO: Write a validator for above asumption ^

	// Create a new directory for the VM disk image
	err := os.Mkdir(fmt.Sprintf("/root/vm-images/vm%v", vmId), 0755)
	if err != nil {
		v.updateVM(vmId, "broken")
		fmt.Fprintf(os.Stderr, "error creating disk image directory for new VM: %v", err)
		return
	}

	// Create new disk image
	err = exec.Command("qemu-img", "create", "-f", "qcow2", "-o", "backing_file=/root/vm-images/base-gentoo-vanilla-v2.img", fmt.Sprintf("/root/vm-images/vm%v/vm%v-gentoo-vanilla-v2.img", vmId, vmId)).Run()
	if err != nil {
		v.updateVM(vmId, "broken")
		fmt.Fprintf(os.Stderr, "error creating disk image for new VM: %v", err)
		return
	}

	// Create our network bridge and configuration
	// TODO: Do we need to lock the datastructure here? We might write network information for a VM that isn't made yet
	t, err := template.ParseFiles("assets/net")
	if err != nil {
		v.updateVM(vmId, "broken")
		fmt.Fprintf(os.Stderr, "error creating template for new VM: %v", err)
		return
	}
	var net bytes.Buffer
	err = t.Execute(&net, v)
	if err != nil {
		v.updateVM(vmId, "broken")
		fmt.Fprintf(os.Stderr, "error executing template for new VM: %v", err)
		return
	}

	// Write the file
	err = ioutil.WriteFile("/etc/conf.d/net", net.Bytes(), 0644)
	if err != nil {
		v.updateVM(vmId, "broken")
		fmt.Fprintf(os.Stderr, "error writing net template for new VM: %v", err)
		return
	}

	// Create the symlink for the bridge
	err = os.Symlink("/etc/init.d/net.lo", fmt.Sprintf("/etc/init.d/net.br%v", vmId))
	if err != nil {
		v.updateVM(vmId, "broken")
		fmt.Fprintf(os.Stderr, "error creating bridge symilnk for new VM: %v", err)
		return
	}

	// Restart network
	err = exec.Command("/etc/init.d/net.enp4s0", "restart").Run()
	if err != nil {
		v.updateVM(vmId, "broken")
		fmt.Fprintln(os.Stderr, "error executing enp4s0 restart for new VM: %v", err)
		return
	}
	err = exec.Command(fmt.Sprintf("/etc/init.d/net.br%v", vmId), "start").Run()
	if err != nil {
		v.updateVM(vmId, "broken")
		fmt.Fprintln(os.Stderr, "error executing bridge restart for new VM: %v", err)
		return
	}

	// Create a new screen/qemu instance
	cmd := exec.Command("screen", "-d", "-m", "-S", fmt.Sprintf("vm%v", vmId), "qemu-system-x86_64", "-nographic", "-enable-kvm", "-cpu", "host", "-curses", "-m", "512M", "-drive", fmt.Sprintf("file=/root/vm-images/vm%v/vm%v-gentoo-vanilla-v2.img,if=virtio", vmId, vmId), "-netdev", fmt.Sprintf("tap,helper=/usr/libexec/qemu-bridge-helper --br=br%v,id=hn0", vmId), "-device", "virtio-net-pci,netdev=hn0,id=nic1", "-append", fmt.Sprintf("root=/dev/vda4 ro vmid=%v", vmId), "-kernel", "/root/vm-images/kernels/vmlinuz-4.1.7-hardened-r1")
	// TODO: Find a way to check the error properly without using .Run() -- CHANGE TO .Start() else it won't work in futuer
	out, err := cmd.Output()
	outStr := string(out)
	if err != nil {
		v.updateVM(vmId, "broken")
		fmt.Fprintln(os.Stderr, "error starting screen session for new VM: %v \r\nadditional: %v", err, outStr)
		return
	}

/*
	// TODO Finish writing part that SSHs in and changes password
	// Lets wait until the VM has its networking set up (timeout if required)
	time.Sleep(60 * time.Second)

	worked := false
	// TODO Determine if best to use a ticker or sleep here
	for i := 0; i < 5; i++ {
		// SSH in and do the final configuraion
		time.Sleep(30 * time.Second)
	}
	if !worked {
		// TODO Write a better cleanup here, as this is clearly taking up space...
		v.updateVM(vmId, "broken")
		fmt.Fprintln(os.Stderr, "error could not connect to SSH within 3 minutes -- manual cleanup required")
		return
	}
	*/

	// Talk to the raspberry pi about getting a new Tor set up
	resp, err := http.Get(fmt.Sprintf("http://10.0.0.5/create/%v", vmId))
	if err != nil {
		v.updateVM(vmId, "broken")
		fmt.Fprintln(os.Stderr, "error talking to torcontrol: %v", err)
		return
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		v.updateVM(vmId, "broken")
		fmt.Fprintln(os.Stderr, "error getting response from torcontrol: %v", err)
		return
	}

	// Determine if the final part, tor stuff, worked
	status := string(body)

	if (status == "invalid" || status == "error") {
		// TODO we should never get here, so handle this more strongly, it's probably an attack?
		fmt.Fprintln(os.Stderr, "error talking to torcontrol, response: %v", err)
		v.updateVM(vmId, "broken")
		return
	}

	// Update VM status to be the onion address
	v.updateVM(vmId, status)
}
