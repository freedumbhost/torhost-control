package main

import (
	"fmt"
	"os"
	"net/http"
	"sync"
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
	if len(v.vms) >= 5 {
		fmt.Fprintf(w, "<h1>Too many VMs are already running. Come back later</h1>")
	} else {
		vmId := v.addVM()
		http.Redirect(w, r, fmt.Sprintf("/view/%v", vmId), http.StatusFound)
	}
}

func viewHandler(w http.ResponseWriter, r *http.Request, v VMInformation) {
	vmId := r.URL.Path[len("/view/"):]
	fmt.Fprintf(w, "<h1>VM created. ID: %v</h1>", vmId)

}

func indexHandler(w http.ResponseWriter, r *http.Request, v VMInformation) {
	fmt.Fprintf(w, "<h1>You got here.</h1>There are currently %v VMs running. <a href='/create'>Create a new VM</a>.", len(v.vms))
}


