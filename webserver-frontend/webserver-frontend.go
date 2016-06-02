package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
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

func (v *VMList) addVM() (vmId int, err error) {
	v.mux.Lock()
	defer v.mux.Unlock()

	vmId = 0
	err = nil
	for i := 100; i < 254; i++ {
		if _, exists := v.Vms[i]; !exists {
			vmId = i
			break
		}
	}

	if vmId == 0 {
		err = errors.New("no free hosts")
		return
	}

	// Instantiate a new VMInformation with the given ID
	vmInfo := VMInformation{Id: vmId}
	v.Vms[vmId] = vmInfo

	return
}

func (v *VMList) updateVM(vmId int, status string, url string) error {
	VMInfo := VMInformation{URL: url, Status: status, Id: vmId}
	v.mux.Lock()
	defer v.mux.Unlock()

	v.Vms[vmId] = VMInfo

	return nil
}

func (v *VMList) updateVMs(newVMList map[int]VMInformation) error {
	v.mux.Lock()
	defer v.mux.Unlock()

	v.Vms = newVMList

	return nil
}

func main() {
	os.Exit(run())
}

func run() int {
	// Create our datastructure
	v := VMList{Vms: make(map[int]VMInformation)}

	// Sync our list of VMs with the hypervisor
	err := syncWithHypervisor(v)
	if err != nil {
		fmt.Printf("Error syncing with hypervisor: %v", err)
		return 1
	}

	// Register handlers for our static files
	registerStaticFiles()

	// TODO Template caching
	// TODO A nicer 404 and 5XX page

	http.HandleFunc("/about", func(w http.ResponseWriter, r *http.Request) {
		aboutHandler(w, r, v)
	})

	// the contact page requires a mutex for processing the input
	var cm sync.Mutex
	http.HandleFunc("/contact", func(w http.ResponseWriter, r *http.Request) {
		contactHandler(w, r, cm)
	})

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

func syncWithHypervisor(v VMList) error {
	// Get the updated list from the hypervisor daemon
	resp, err := http.Get("http://10.0.5.20/sync")
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Error talking to hypervisor-daemon (sync) - %v", time.Now(), err))
		return errors.New("talking to hypervisor-daemon")
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Error reading response from hypervisor-daemon (sync) - %v", time.Now(), err))
		return errors.New("Error reading response from hypervisor-daemon")
	}

	var InterVms map[string]VMInformation
	err = json.Unmarshal(body, &InterVms)
	if err != nil {
		return err
	}

	Vms := make(map[int]VMInformation)
	for _, value := range InterVms {
		Vms[value.Id] = value
	}

	v.updateVMs(Vms)
	return nil
}

func registerStaticFiles() {
	// Loop over the static directory
	err := filepath.Walk("static/", func(path string, f os.FileInfo, err error) error {
		if !f.IsDir() {
			// Get the filename without the prefix
			filename := path[len("static"):]
			// Register a handler to do the right thing for this
			http.HandleFunc(filename, func(w http.ResponseWriter, r *http.Request) {
				// Cache our static files for a while
				w.Header().Set("Cache-Control", "public, max-age=3600")
				// Output the file
				http.ServeFile(w, r, fmt.Sprintf("static/%v", r.URL.Path))
			})
		}
		return nil
	})
	if err != nil {
		panic("Could not walk the filesystem for static files")
	}
}

func createHandler(w http.ResponseWriter, r *http.Request, v VMList) {
	fmt.Println(fmt.Sprintf("[%v] %v", time.Now(), r.URL.Path))

	// Check we don't have too many VMs running
	if len(v.Vms) >= 10 {
		// Render the "too many" template
		t, err := template.ParseFiles("templates/create-toomany.html")
		if err != nil {
			fmt.Println(fmt.Sprintf("[%v] Could parse template - %v", time.Now(), err))
			http.Error(w, "Error", http.StatusInternalServerError)
			return
		}
		err = t.Execute(w, nil)
		if err != nil {
			fmt.Println(fmt.Sprintf("[%v] Could execute template - %v", time.Now(), err))
			http.Error(w, "Error", http.StatusInternalServerError)
			return
		}
		// Template rendered, we're done
		return
	}

	// Add it to our internal tracking
	vmId, err := v.addVM()
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Could not execute v.addVM() - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	// Talk to the hypervisor about creating the new VM
	resp, err := http.Get(fmt.Sprintf("http://10.0.5.20/create/%v", vmId))
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Error talking to hypervisor-daemon (create) - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Error reading response from hypervisor-daemon (create) - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	status := string(body)
	if status != "creating" {
		fmt.Println(fmt.Sprintf("[%v] Unexpected response from hypervisor-daemon (create) - %v", time.Now(), status))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/view/%v", vmId), http.StatusFound)
}

func viewHandler(w http.ResponseWriter, r *http.Request, v VMList) {
	fmt.Println(fmt.Sprintf("[%v] %v", time.Now(), r.URL.Path))
	vmIdString := r.URL.Path[len("/view/"):]

	// Validate the vmId
	vmId, err := strconv.Atoi(vmIdString)
	if err != nil {
		http.Error(w, "Invalid VM specified", http.StatusInternalServerError)
		return
	}
	if vmId < 100 || vmId > 254 {
		http.Error(w, "Invalid VM specified", http.StatusInternalServerError)
		return
	}

	// Resync the status of this VM with the hypervisor
	resp, err := http.Get(fmt.Sprintf("http://10.0.5.20/view/%v", vmId))
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Error talking to hypervisor-daemon (view) - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Error reading response from hypervisor-daemon (view) - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	status := string(body)

	// Parse the status so we can build the proper representation
	url := ""
	if !(status == "creating" || status == "broken" || status == "invalid") {
		url = status
		status = "complete"
	}
	v.updateVM(vmId, status, url)

	// Render the template
	t, err := template.ParseFiles("templates/view.html")
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Could parse template - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	err = t.Execute(w, v.Vms[vmId])
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Could execute template - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
}

func aboutHandler(w http.ResponseWriter, r *http.Request, v VMList) {
	fmt.Println(fmt.Sprintf("[%v] %v", time.Now(), r.URL.Path))

	// Render the template
	t, err := template.ParseFiles("templates/about.html")
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Could parse template - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	err = t.Execute(w, nil)
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Could execute template - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
}

func contactHandler(w http.ResponseWriter, r *http.Request, cm sync.Mutex) {
	fmt.Println(fmt.Sprintf("[%v] %v", time.Now(), r.URL.Path))

	// Check for form submission, etc
	err := r.ParseForm()
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Could parse form data - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	// TODO Investigate a better way to display the information
	sent_message := "none"

	// Technically they might have submitted an empty message, but we'll ignore that possibility
	if val, ok := r.Form["message"]; ok {
		// TODO Check if the message is blank
		if val[0] == "" {
			sent_message = "empty"
		} else {
			// They submitted a message. Process it, then display a notification
			cm.Lock()
			defer cm.Unlock()

			// Just write it to a temporary file
			file, err := ioutil.TempFile("messages/", "msg-")
			if err != nil {
				fmt.Println(fmt.Sprintf("[%v] Could create temporary file - %v", time.Now(), err))
				http.Error(w, "Error", http.StatusInternalServerError)
				return
			}

			// Write out the data
			_, err = file.Write([]byte(val[0]))
			if err != nil {
				fmt.Println(fmt.Sprintf("[%v] Could write temporary file form data - %v", time.Now(), err))
				http.Error(w, "Error", http.StatusInternalServerError)
				return
			}

			sent_message = "yes"
		}
	}

	// Render the template
	t, err := template.ParseFiles("templates/contact.html")
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Could parse template - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	err = t.Execute(w, sent_message)
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Could execute template - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
}

func indexHandler(w http.ResponseWriter, r *http.Request, v VMList) {
	fmt.Println(fmt.Sprintf("[%v] %v", time.Now(), r.URL.Path))

	// Render the template
	t, err := template.ParseFiles("templates/index.html")
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Could parse template - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	err = t.Execute(w, nil)
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Could execute template - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
}
