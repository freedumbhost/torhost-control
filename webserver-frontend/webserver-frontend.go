package main

import (
	"fmt"
	"os"
	"net/http"
	"sync"
	"time"
	"io/ioutil"
)

type VMInformation struct {
	mux sync.Mutex
	// TODO: Replace this map with more useful information (creation date etc)
	vms map[int]string
}

func (v *VMInformation) addVM() (vmId int) {
	v.mux.Lock()

	// Find the next ID we can use
	vmId = 100 + len(v.vms)
	v.vms[vmId] = "Placeholder"

	v.mux.Unlock()
	return
}

func main() {
	os.Exit(run())
}

func run() (int) {
	// Create our datastructure
	v := VMInformation{vms: make(map[int]string)}
	v.addVM() // Create the VM we know exists as VM 100

	http.HandleFunc("/create", func(w http.ResponseWriter, r *http.Request) {
		createHandler(w, r, v)
	})

	http.HandleFunc("/view/", func(w http.ResponseWriter, r *http.Request) {
		viewHandler(w, r, v)
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		indexHandler(w, r, v)
	})

	http.ListenAndServe(":80", nil)

	return 0
}

func createHandler(w http.ResponseWriter, r *http.Request, v VMInformation) {
	fmt.Println("[%v] %v", time.Now(), r.URL.Path)
	if len(v.vms) >= 5 {
		fmt.Fprintf(w, "<h1>Too many VMs are already running. Come back later</h1>")
	} else {
		vmId := v.addVM()
		// fire off the request
		resp, err := http.Get(fmt.Sprintf("http://10.0.5.20/create/%v", vmId))
		if err != nil {
			fmt.Fprintln(os.Stderr, "error talking to hypervisor-daemon (create): %v", err)
			fmt.Fprintf(w, "shits fucked m8, come back later")
			return
		}
		defer resp.Body.Close()
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error reading response from hypervisor-daemon (create): %v", err)
			fmt.Fprintf(w, "shits fucked m8, come back later")
			return
		}

		status := string(body)
		if status != "creating" {
			fmt.Fprintln(os.Stderr, "unexpected response from create hypervisor-daemon: %v", status)
			fmt.Fprintf(w, "shits fucked m8, come back later")
			return
		}

		http.Redirect(w, r, fmt.Sprintf("/view/%v", vmId), http.StatusFound)
	}
}

func viewHandler(w http.ResponseWriter, r *http.Request, v VMInformation) {
	fmt.Println("[%v] %v", time.Now(), r.URL.Path)
	vmId := r.URL.Path[len("/view/"):]
	fmt.Fprintf(w, "<h1>VM created. ID: %v</h1>", vmId)

	// find out what the status is and print it
	resp, err := http.Get(fmt.Sprintf("http://10.0.5.20/view/%v", vmId))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error talking to hypervisor-daemon: %v", err)
		fmt.Fprintf(w, "shits fucked m8, come back later")
		return
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error reading response from hypervisor-daemon: %v", err)
		fmt.Fprintf(w, "shits fucked m8, come back later")
		return
	}

	status := string(body)

	fmt.Fprintf(w, "here's a janky status of the VM if it's not created, or the hostname if it is: %v", status)
	fmt.Fprintf(w, "login as root@whatever, password is: emuguestpassword -- you're welcome to change this (please do)")
	fmt.Fprintf(w, "anything likely to attract LEA, I delete your VM. pls no cp. remove my SSH key from root's authorized keys, I delete your VM (for now, I will remove this restriction later)")

}

func indexHandler(w http.ResponseWriter, r *http.Request, v VMInformation) {
	fmt.Println("[%v] %v", time.Now(), r.URL.Path)
	fmt.Fprintf(w, "<h1>Free Dumb Hosting</h1>There are currently %v VMs running. <a href='/create'>Create a new VM</a>. no illegal stuff pls, no cp etc. <br> more features, like being able to host a website instead of just a shell, coming later", len(v.vms))
}


