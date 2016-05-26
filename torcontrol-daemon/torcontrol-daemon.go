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
	"time"
)

// TODO Determine if this needs to be a struct or we can just use a global variable
type TorRCStruct struct {
	Mux sync.Mutex
}

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
	v := TorRCStruct{}

	http.HandleFunc("/create/", func(w http.ResponseWriter, r *http.Request) {
		createHandler(w, r, v)
	})

	// IMPORTANT OMG
	// Only ever bind to one address!
	http.ListenAndServe("10.0.0.5:80", nil)

	return 0
}

func createHandler(w http.ResponseWriter, r *http.Request, v TorRCStruct) {
	// TODO In the future we will allow more than just sshd port to be a hidden service

	// Lock to be safe
	v.Mux.Lock()
	defer v.Mux.Unlock()

	// Get the ID of the new VM
	vmIdStr := r.URL.Path[len("/create/"):]
	vmId, err := strconv.Atoi(vmIdStr)
	// Check whether the ID is valid
	if err != nil {
		fmt.Fprintf(w, "invalid")
		return
	}

	// TODO More validation for if it already exists rather than just error out later

	// TODO Add network configuration
	t, err := template.ParseFiles("assets/networks-vlan")
	if err != nil {
		fmt.Fprintf(w, "error")
		fmt.Fprintf(os.Stderr, "error creating template for new VM: %v", err)
		return
	}
	var net bytes.Buffer
	err = t.Execute(&net, SingleVMInformation{Id: vmId})
	if err != nil {
		fmt.Fprintf(w, "error")
		fmt.Fprintf(os.Stderr, "error executing template for new VM: %v", err)
		return
	}
	// Write net file
	err = ioutil.WriteFile(fmt.Sprintf("/etc/network/interfaces.d/vlan%v", vmId), net.Bytes(), 0644)
	if err != nil {
		fmt.Fprintf(w, "error")
		fmt.Fprintf(os.Stderr, "error writing net template for new VM: %v", err)
		return
	}

	// Restart networking
	err = exec.Command("/etc/init.d/networking", "restart").Run()
	if err != nil {
		// TODO More graceful handling of this. If the network is down, how can we talk back?
		fmt.Fprintf(w, "error")
		fmt.Fprintln(os.Stderr, "error executing networking restart for new VM: %v", err)
		return
	}

	// To generate the new torrc we nee the list of every VM, which we can get by looking in /etc/network/interfaces.d
	vmsfiles, err := filepath.Glob("/etc/network/interfaces.d/vlan*")
	if err != nil {
		fmt.Fprintf(w, "error")
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

	// Generate new torrc
	t, err = template.ParseFiles("assets/torrc")
	if err != nil {
		fmt.Fprintf(w, "error")
		fmt.Fprintf(os.Stderr, "error creating torrc template for new VM: %v", err)
	}

	var torrc bytes.Buffer
	err = t.Execute(&torrc, vms)
	if err != nil {
		fmt.Fprintf(w, "error")
		fmt.Fprintf(os.Stderr, "error executing torrc template for new VM: %v", err)
		return
	}

	// Write new torrc
	err = ioutil.WriteFile("/etc/tor/torrc", torrc.Bytes(), 0644)
	if err != nil {
		fmt.Fprintf(w, "error")
		fmt.Fprintf(os.Stderr, "error writing torrc template for new VM: %v", err)
		return
	}

	// Restart tor to reload torrc
	// TODO just reparse file instead of restart
	err = exec.Command("/etc/init.d/tor", "restart").Run()
	if err != nil {
		// TODO More graceful handling of this. If tor is down, HOLY SHIT FIRE
		fmt.Fprintf(w, "error")
		fmt.Fprintln(os.Stderr, "error executing tor restart for new VM: %v", err)
		return
	}

	// Return the hostname of the new hidden service
	time.Sleep(5 * time.Second) // Give it some time to actually generate the new private key
	// TODO poll on creation instead of sleep ^^

	buf, err := ioutil.ReadFile(fmt.Sprintf("/var/lib/tor/guest-%v/hostname", vmId))
	if err != nil {
		// TODO probably just sleep more if this happens, or just fix above TODO ^^
		fmt.Fprintf(w, "error")
		fmt.Fprintln(os.Stderr, "error fetching hostname for hidden service", err)
	}

	// If we get to here, we're DONE
	// TODO We should really return success + the hostname
	fmt.Fprintf(w, string(buf))
}
