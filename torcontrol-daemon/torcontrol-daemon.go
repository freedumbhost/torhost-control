package main

import (
	"fmt"
	"os"
	"os/exec"
	"net/http"
	"sync"
	"text/template"
	"strconv"
	"bytes"
	"io/ioutil"
	"path/filepath"
	"strings"
)

type VMInformation struct {
	mux sync.Mutex
	// This map is id => state (should be an enum I guess)
	Vms map[int]string
}

type SingleVMInformation struct {
	Id int
}

func main() {
	os.Exit(run())
}

func run() (int) {
	// Create our datastructure
	v := sync.Mutex{}

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

func viewHandler(w http.ResponseWriter, r *http.Request, v sync.Mutex) {
	// Get the ID of the new VM
	vmIdStr := r.URL.Path[len("/create/"):]
	vmId, err := strconv.Atoi(vmIdStr)
	// Check whether the ID is valid
	if err != nil {
		fmt.Fprintf(w, "invalid")
		return
	}

	if (vmId < 50 || vmId > 255) {
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
	err = t.Execute(&net, SingleVMInformation{Id: vmId})
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

	// To generate the new torrc we nee the list of every VM, which we can get by looking in /etc/network/interfaces.d
	vmsfiles, err := filepath.Glob("/etc/network/interfaces.d/vlan*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error globbing for VMs: %v", err)
		return
	}

	vms := VMInformation{Vms: make(map[int]string)}

	for _, f := range vmsfiles {
		fid := strings.Split(f, "vlan")
		i, err := strconv.Atoi(fid[1])
		if err == nil {
			vms.Vms[i] = "complete"
		} // else it probably was something we can ignore
	}

	// Configure ip tables
	// TODO rewrite so this doesn't require a reload
	t, err = template.ParseFiles("assets/iptables")
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
	//err = exec.Command("iptables-restore", "/etc/iptables").Run()
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
	err = exec.Command("pkill", "-SIGHUP", "/usr/bin/tor").Run()
	if err != nil {
		// TODO More graceful handling of this. If tor is down, HOLY SHIT FIRE
		fmt.Fprintln(os.Stderr, "error executing tor restart for new VM: %v", err)
		return
	}

}
