package main

import (
	"bytes"
	"fmt"
	"github.com/garyburd/redigo/redis"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	Status    string
	Id        int
	OpenPorts map[string]string
}

var configLock sync.Mutex

func main() {
	os.Exit(run())
}

func run() int {
	// Create our datastructures
	v := sync.Mutex{}
	configLock = sync.Mutex{}

	// Connect here rather than in the handler so that we can ensure we can connect and exit if need be
	{
		// Scope our variable to ensure no one accidently uses the connection
		redisCon, err := redis.Dial("tcp", "10.0.5.20:6379")
		if err != nil {
			fmt.Printf("Could not connect to redis database: %v", err)
			return 1
		}
		go redisPubSubHandle(redisCon)
		defer redisCon.Close()

	}

	http.HandleFunc("/create/", func(w http.ResponseWriter, r *http.Request) {
		createHandler(w, r, v)
	})

	http.HandleFunc("/view/", func(w http.ResponseWriter, r *http.Request) {
		viewHandler(w, r, v)
	})

	// IMPORTANT OMG
	// Only ever bind to one address!
	http.ListenAndServe("10.0.0.5:80", nil)

	return 0
}

func redisPubSubHandle(redisCon redis.Conn) {
	psc := redis.PubSubConn{Conn: redisCon}
	psc.Subscribe("openport")
	psc.Subscribe("deletevm")

	for {
		// TODO Can we use an if/continue instead of a switch?
		switch v := psc.Receive().(type) {
		case redis.Message:
			switch v.Channel {
			case "openport":
				// Lets tell Tor to regenerate its configuration (which happens without the state of the previous message)
				// TODO: We should just add the port to the config, not regenerate it from scratch, probably
				go rewriteConfig()
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

				go deleteVm(vmId)
			}
		}
		fmt.Println(fmt.Sprintf("[%v] Got a PUBSUB message", time.Now()))
	}
}

func deleteVm(vmId int) {
	if vmId < 50 || vmId > 255 {
		return
	}

	// delete the networking file
	err := os.Remove(fmt.Sprintf("/etc/network/interfaces.d/vlan%v", vmId))
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not delete networking interface (vmId: %v): %v", vmId, err)
	}

	// regenerate configuration
	rewriteConfig()

	// now clean up the extra files in /var/lib/tor
	err = os.Remove(fmt.Sprintf("/var/lib/tor/guest-%v", vmId))
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not delete tor datadir (vmId: %v): %v", vmId, err)
	}
}

func rewriteConfig() {
	configLock.Lock()
	defer configLock.Unlock()

	// To generate the new torrc we need the list of every VM, which we can get by looking in /etc/network/interfaces.d
	vmsfiles, err := filepath.Glob("/etc/network/interfaces.d/vlan*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error globbing for VMs: %v", err)
		return
	}

	vms := VMList{Vms: make(map[int]VMInformation)}

	// A variable for whether we should bother talking to redis
	doRedis := true
	redisCon, err := redis.Dial("tcp", "10.0.5.20:6379")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error connecting to redis: %v", err)
		doRedis = false
	}

	for _, f := range vmsfiles {
		fid := strings.Split(f, "vlan")
		i, err := strconv.Atoi(fid[1])
		if err == nil {
			// We have a valid ID we should build a VMInformation for
			var vminfo VMInformation
			vminfo = VMInformation{Id: i, Status: "complete"}
			// Check with redis to see if we need to have an open port
			if doRedis {
				vminfo.OpenPorts = make(map[string]string)
				inter, err := redis.Strings(redisCon.Do("SMEMBERS", fmt.Sprintf("vm:%v:hostedports", i)))
				for _, f := range inter {
					vminfo.OpenPorts[f] = f
				}
				if err != nil {
					fmt.Fprintln(os.Stderr, "error talking to redis: %v", err)
				}
				// We can ignore an error since the map is still intialized
				vms.Vms[i] = vminfo
			}
			// Add it to our VMList
		} // else it probably was something we can ignore
	}

	// Configure ip tables
	// TODO rewrite so this doesn't require a reload
	t, err := template.ParseFiles("assets/iptables")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating iptables template for new VM: %v", err)
		return
	}

	var iptables bytes.Buffer
	err = t.Execute(&iptables, vms)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error executing iptables template for new VM: %v", err)
		return
	}

	// Write new iptables file
	err = ioutil.WriteFile("/etc/iptables", iptables.Bytes(), 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error writing iptables template for new VM: %v", err)
		return
	}

	// Reload iptables
	err = exec.Command("iptables-restore", "/etc/iptables").Run()
	if err != nil {
		// TODO More graceful handling of this. If iptables is down, HOLY SHIT FIRE, like shutdown -h now
		fmt.Fprintln(os.Stderr, "error executing iptables restore for new VM: %v", err)
		return
	}

	// Generate new torrc
	t, err = template.ParseFiles("assets/torrc")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating torrc template for new VM: %v", err)
		return
	}

	var torrc bytes.Buffer
	err = t.Execute(&torrc, vms)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error executing torrc template for new VM: %v", err)
		return
	}

	// Write new torrc
	err = ioutil.WriteFile("/etc/tor/torrc", torrc.Bytes(), 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error writing torrc template for new VM: %v", err)
		return
	}

	// Send SIGHUP to Tor
	// From docs: The signal instructs Tor to reload its configuration (including closing and reopening logs), and kill and restart its helper processes if applicable.
	// TODO Look into using control port to make changes without a reload required
	err = exec.Command("pkill", "-SIGHUP", "tor$").Run()
	if err != nil {
		// TODO More graceful handling of this. If tor is down, HOLY SHIT FIRE
		fmt.Fprintln(os.Stderr, "error executing tor restart for new VM: %v", err)
		return
	}

}

func viewHandler(w http.ResponseWriter, r *http.Request, v sync.Mutex) {
	fmt.Println(fmt.Sprintf("[%v] %v", time.Now(), r.URL.Path))
	// Get the ID of the new VM
	vmIdStr := r.URL.Path[len("/view/"):]
	vmId, err := strconv.Atoi(vmIdStr)
	// Check whether the ID is valid
	if err != nil {
		fmt.Fprintf(w, "invalid")
		return
	}

	if vmId < 50 || vmId > 255 {
		fmt.Fprintf(w, "invalid")
		return
	}

	// There's a chance it doesn't exist yet, but meh, we can't do much
	buf, err := ioutil.ReadFile(fmt.Sprintf("/var/lib/tor/guest-%v/hostname", vmId))
	if err != nil {
		fmt.Fprintf(w, "unknown")
		fmt.Fprintln(os.Stderr, "error fetching hostname for hidden service", err)
		return
	}

	// TODO We should really return success + the hostname (json n shit yo)
	fmt.Fprintf(w, string(buf))
}

func createHandler(w http.ResponseWriter, r *http.Request, v sync.Mutex) {
	fmt.Println(fmt.Sprintf("[%v] %v", time.Now(), r.URL.Path))
	// TODO In the future we will allow more than just sshd port to be a hidden service
	// Lock to be safe (unlock in the actual create goroutine)
	v.Lock()

	// Get the ID of the new VM
	vmIdStr := r.URL.Path[len("/create/"):]
	vmId, err := strconv.Atoi(vmIdStr)
	// Check whether the ID is valid
	if err != nil {
		fmt.Fprintf(w, "invalid")
		return
	}

	// TODO More validation for if it already exists rather than just error out later

	// create in background
	go create(vmId, v)
	fmt.Fprintf(w, "creating")
}

func create(vmId int, v sync.Mutex) {
	// remove the lock when we're done
	defer v.Unlock()

	// Add network configuration
	t, err := template.ParseFiles("assets/networks-vlan")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating template for new VM: %v", err)
		return
	}
	var net bytes.Buffer
	err = t.Execute(&net, VMInformation{Id: vmId})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error executing template for new VM: %v", err)
		return
	}
	// Write net file
	err = ioutil.WriteFile(fmt.Sprintf("/etc/network/interfaces.d/vlan%v", vmId), net.Bytes(), 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error writing net template for new VM: %v", err)
		return
	}

	// Instead of restarting network, lets just run the commands to bring up that one vlan
	err = exec.Command("vconfig", "add", "eth0", fmt.Sprintf("%v", vmId)).Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error running vconfig", err)
		return
	}
	err = exec.Command("ifconfig", fmt.Sprintf("eth0.%v", vmId), "up").Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error running ifconfig up", err)
		return
	}
	err = exec.Command("ifconfig", fmt.Sprintf("eth0.%v", vmId), fmt.Sprintf("10.0.%v.5", vmId), "netmask", "255.255.255.0").Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error running ifconfig configuration", err)
		return
	}

	// Now we just create it
	rewriteConfig()
}
