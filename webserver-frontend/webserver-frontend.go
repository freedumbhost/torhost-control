package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/garyburd/redigo/redis"
	"github.com/gorilla/mux"
	"gopkg.in/boj/redistore.v1"
	"html/template"
	"io/ioutil"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Our global session store
var store *redistore.RediStore

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

func (v *VMList) removeVM(vmId int) error {
	v.mux.Lock()
	defer v.mux.Unlock()

	delete(v.Vms, vmId)

	return nil
}

func main() {
	os.Exit(run())
}

func run() int {
	// Connect to redis
	redisCon, err := redis.Dial("tcp", "10.0.5.20:6379")
	if err != nil {
		fmt.Printf("Could not connect to redis database: %v", err)
		return 1
	}
	defer redisCon.Close()

	// Set up our session configuration
	authKey, err := ioutil.ReadFile("config/auth.key")
	if err != nil {
		fmt.Println("Could not read auth.key")
		return 1
	}
	encKey, err := ioutil.ReadFile("config/enc.key")
	if err != nil {
		fmt.Println("Could not read enc.key")
		return 1
	}
	store, err = redistore.NewRediStore(10, "tcp", "10.0.5.20:6379", "", authKey, encKey)
	if err != nil {
		fmt.Println("Could not start redis session store")
		return 1
	}
	//store = sessions.NewFilesystemStore("", authKey, encKey) // Filesystem store

	// Create our datastructure
	v := VMList{Vms: make(map[int]VMInformation)}

	// Sync our list of VMs with the hypervisor
	err = syncWithHypervisor(&v)
	if err != nil {
		fmt.Printf("Error syncing with hypervisor: ", err)
		return 1
	}

	// Connect to redis for PUBSUB duties
	{
		// Scope our variable to ensure no one accidently uses the connection
		redisCon, err := redis.Dial("tcp", "10.0.5.20:6379")
		if err != nil {
			fmt.Printf("Could not connect to redis database: %v", err)
			return 1
		}
		go redisPubSubHandle(redisCon, &v)
		defer redisCon.Close()
	}

	// TODO Template caching
	// TODO A nicer 404 and 5XX page
	// These functions repeat a lot, rewrite (globals ftw)

	r := mux.NewRouter()

	// Register handlers for our static files
	registerStaticFiles(r)

	r.HandleFunc("/about", func(w http.ResponseWriter, r *http.Request) {
		aboutHandler(w, r, v)
	})

	// the contact page requires a mutex for processing the input
	var cm sync.Mutex
	r.HandleFunc("/contact", func(w http.ResponseWriter, r *http.Request) {
		contactHandler(w, r, cm)
	})

	// GET for /create, displays a form
	r.HandleFunc("/create", func(w http.ResponseWriter, r *http.Request) {
		createGetHandler(w, r, v)
	}).Methods("GET")

	// POST for /create, does the work
	r.HandleFunc("/create", func(w http.ResponseWriter, r *http.Request) {
		createPostHandler(w, r, v, redisCon)
	}).Methods("POST")

	r.HandleFunc("/view/{id:[0-9]+}", func(w http.ResponseWriter, r *http.Request) {
		viewHandler(w, r, v)
	})

	r.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		loginHandler(w, r, v, redisCon)
	})

	r.HandleFunc("/manage", func(w http.ResponseWriter, r *http.Request) {
		manageHandler(w, r, v, redisCon)
	})

	r.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		indexHandler(w, r, v)
	})

	http.ListenAndServe(":80", r)

	return 0
}

func syncWithHypervisor(v *VMList) error {
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

	fmt.Printf("[%v] Synced with hypervisor. Currently listing we have %v VMs.\r\n", time.Now(), len(v.Vms))
	return nil
}

func registerStaticFiles(r *mux.Router) {
	// Loop over the static directory
	err := filepath.Walk("static/", func(path string, f os.FileInfo, err error) error {
		if !f.IsDir() {
			// Get the filename without the prefix
			filename := path[len("static"):]
			// Register a handler to do the right thing for this
			r.HandleFunc(filename, func(w http.ResponseWriter, r *http.Request) {
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

func redisPubSubHandle(redisCon redis.Conn, v *VMList) {
	psc := redis.PubSubConn{Conn: redisCon}
	psc.Subscribe("deletevm")

	for {
		// TODO Can we use an if/continue instead of a switch?
		switch m := psc.Receive().(type) {
		case redis.Message:
			switch m.Channel {
			case "deletevm":
				// Parse out the ID and if required, do the deed
				vmId, err := strconv.Atoi(string(m.Data))
				// Check whether the ID is valid
				if err != nil {
					continue
				}
				if vmId < 50 || vmId > 255 {
					return
				}

				go deleteVm(vmId, v)
			}
		}
		fmt.Println(fmt.Sprintf("[%v] Got a PUBSUB message", time.Now()))
	}
}

func deleteVm(vmId int, v *VMList) {
	if vmId < 50 || vmId > 255 {
		return
	}

	redisCon, err := redis.Dial("tcp", "10.0.5.20:6379")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error connecting to redis: %v", err)
		return
	}

	// delete the hostedposts/password rows
	redisCon.Do("DEL", fmt.Sprintf("vm:%v:password", vmId))
	redisCon.Do("DEL", fmt.Sprintf("vm:%v:hostedports", vmId))

	// Clear out the session entries
	sessions, err := redis.Strings(redisCon.Do("KEYS", "session_*"))
	for _, key := range sessions {
		redisCon.Do("DEL", key)
	}

	// IT'S JUST A PRANK BRO

	// remove it from our internal object so it can be reallocated
	v.removeVM(vmId)
}

func loginHandler(w http.ResponseWriter, r *http.Request, v VMList, redisCon redis.Conn) {
	fmt.Println(fmt.Sprintf("[%v] %v", time.Now(), r.URL.Path))

	session, err := store.Get(r, "torcontrol-session")
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Error opening session (login)  - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	if session.Values["vmId"] != nil {
		// A session is already created, lets just redirect
		http.Redirect(w, r, "/manage", http.StatusFound)
		return
	}
	// Check if we need to process the login
	err = r.ParseForm()
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Could parse form data (login) - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	login_attempt := false
	empty := false

	if val, ok := r.Form["password"]; ok {
		if val[0] == "" {
			empty = true
		}
	} else {
		empty = true
	}

	if val, ok := r.Form["vmid"]; ok {
		if val[0] == "" {
			empty = true
		}
	} else {
		empty = true
	}

	if !empty {
		irply, err := redisCon.Do("GET", fmt.Sprintf("vm:%v:password", r.Form["vmid"][0]))
		if err != nil {
			fmt.Println(fmt.Sprintf("[%v] Error from redis (login) - %v", time.Now(), err))
			http.Error(w, "Internal Error", http.StatusInternalServerError)
			return
		}
		rply, err := redis.String(irply, nil)
		if err != nil && err != redis.ErrNil {
			fmt.Println(fmt.Sprintf("[%v] Error converting from redis to string (login) - %v", time.Now(), err))
			http.Error(w, "Internal Error", http.StatusInternalServerError)
			return
		}
		if err != nil {
			http.Error(w, "No VM found with that ID", http.StatusNotFound)
			return
		}
		// TODO: Hashing (though not a major issue since they're randomly generated)
		if r.Form["password"][0] == rply {
			session.Values["vmId"] = r.Form["vmid"][0]
			err := session.Save(r, w)
			if err != nil {
				fmt.Println(fmt.Sprintf("[%v] Could not save session - %v", time.Now(), err))
				http.Error(w, "Error", http.StatusInternalServerError)
				return
			}
			// Redirect to logged in page
			http.Redirect(w, r, "/manage", http.StatusFound)
			return // We're done!
		}
		login_attempt = true
	}

	// Render the template
	t, err := template.ParseFiles("templates/login.html")
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Could parse template - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	err = t.Execute(w, login_attempt)
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Could execute template - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

}

/*
func deleteHandler(w http.ResponseWriter, r *http.Request, v VMList, redisCon redis.Conn) {
	fmt.Println(fmt.Sprintf("[%v] %v", time.Now(), r.URL.Path))

	session, err := store.Get(r, "torcontrol-session")
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Error opening session (manage)  - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	if session.Values["vmId"] == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	// Convert it to an ID for stuff
	vmId, err := strconv.Atoi(session.Values["vmId"].(string))
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Error casting vmid (manage)  - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	// If it's not a POST, just redirect back to the manage form
	if r.Method != "POST" {
		http.Redirect(w, r, "/manage", http.StatusFound)
		return
	}

	// Doesn't matter what the POST data is/was

	// Perform the PUBSUB first

	// Delete the password out of redis

	// TODO: Destroy all sessions that have this VM ID so they don't persist

}
*/

func manageHandler(w http.ResponseWriter, r *http.Request, v VMList, redisCon redis.Conn) {
	// We only have a single handler for POST and GET, because we display the same form either way
	fmt.Println(fmt.Sprintf("[%v] %v", time.Now(), r.URL.Path))

	session, err := store.Get(r, "torcontrol-session")

	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Error opening session (manage)  - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	if session.Values["vmId"] == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	// Convert it to an ID for stuff
	vmId, err := strconv.Atoi(session.Values["vmId"].(string))
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Error casting vmid (manage)  - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	// Do the post action if we need to
	// Currently, only POST is the stuff to activate the port 80 stuff
	// Check if we need to process the login
	if r.Method == "POST" {
		err = r.ParseForm()
		if err == nil {
			var command string

			if _, ok := r.Form["port80"]; ok {
				command = "SADD"
			} else {
				command = "SREM"
			}
			// Either remove or add the port in the database
			_, err := redisCon.Do(command, fmt.Sprintf("vm:%v:hostedports", vmId), "80")
			if err != nil {
				fmt.Println(fmt.Sprintf("[%v] Error from redis (manage) - %v", time.Now(), err))
				http.Error(w, "Error", http.StatusInternalServerError)
				return
			}

			// If we get here, we should trigger a PUBSUB to activate the change in state, in either direction
			_, err = redisCon.Do("PUBLISH", "openport", fmt.Sprintf("%v:%v", vmId, 80))
			if err != nil {
				fmt.Println(fmt.Sprintf("[%v] Error from redis (manage) - %v", time.Now(), err))
				http.Error(w, "Error", http.StatusInternalServerError)
				return
			}
		}
	}

	// Retrive the key from the database for whether port 80 is open
	port80, err := redis.Bool(redisCon.Do("SISMEMBER", fmt.Sprintf("vm:%v:hostedports", vmId), "80"))
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Error from redis (manage) - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	// Render the template
	t, err := template.ParseFiles("templates/manage.html")
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Could parse template - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	templateData := struct {
		VMInfo     VMInformation
		Port80Open bool
	}{
		v.Vms[vmId],
		port80,
	}
	err = t.Execute(w, templateData)

	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Could execute template - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
}

func createGetHandler(w http.ResponseWriter, r *http.Request, v VMList) {
	fmt.Println(fmt.Sprintf("[%v] %v (GET)", time.Now(), r.URL.Path))

	// Start a session so we have a randomly generated password to give them
	session, err := store.Get(r, "torcontrol-session")
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Error opening session (create-get)  - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	session.Values["randomString"], err = randomString(20, nil)
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Error generating random token (create-get)  - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	// Save the session
	err = session.Save(r, w)
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Could not save session (create-get) - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	// Render the template
	t, err := template.ParseFiles("templates/create.html")
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Could parse template - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	templateData := struct {
		NumberOfVMs  int
		RandomString string
	}{
		len(v.Vms),
		session.Values["randomString"].(string),
	}
	err = t.Execute(w, templateData)
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Could execute template - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
}

func createPostHandler(w http.ResponseWriter, r *http.Request, v VMList, redisCon redis.Conn) {
	fmt.Println(fmt.Sprintf("[%v] %v (POST)", time.Now(), r.URL.Path))

	// Validate we have a session and key
	session, err := store.Get(r, "torcontrol-session")

	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Error opening session (create-post)  - %v", time.Now(), err))
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}

	if (session.Values["randomString"] == nil) || (len(session.Values["randomString"].(string)) != 20) {
		// Did not have a valid session, lets just give an error page, they can go back and try again
		fmt.Println(fmt.Sprintf("[%v] Error validating session (create-post) - session value: %v", time.Now(), session.Values["randomString"]))
		http.Error(w, "Error - session invalid", http.StatusInternalServerError)
		return
	}

	// Check we don't have too many VMs running
	if len(v.Vms) >= 20 {
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

	// Add the key to redis
	rply, err := redisCon.Do("SET", fmt.Sprintf("vm:%v:password", vmId), session.Values["randomString"].(string))
	if err != nil {
		fmt.Println(fmt.Sprintf("[%v] Error from redis (create) - %v", time.Now(), err))
		http.Redirect(w, r, fmt.Sprintf("/view/%v?error=session", vmId), http.StatusFound)
		return
	}

	if rply != "OK" {
		fmt.Println(fmt.Sprintf("[%v] Unexpected response from redis (create) - %v", time.Now(), rply))
		http.Redirect(w, r, fmt.Sprintf("/view/%v?error=session", vmId), http.StatusFound)
		return
	}

	// Assume the key is added, yay!
	// TODO: Do we need to check the response _ above?

	http.Redirect(w, r, fmt.Sprintf("/view/%v", vmId), http.StatusFound)
}

func viewHandler(w http.ResponseWriter, r *http.Request, v VMList) {
	fmt.Println(fmt.Sprintf("[%v] %v", time.Now(), r.URL.Path))

	// TODO: Deal with the case where ?error=true, meaning there was a redis error

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

/**
 * Generate a random string of a given length with a given alphabet
 * If letters parameter is empty, use a default alphabet of ascii runes
 */
func randomString(length int, letters []rune) (string, error) {
	if letters == nil {
		letters = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890")
	}

	b := make([]rune, length)
	for i := range b {
		rint, err := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
		// bail out if error
		if err != nil {
			return "", err
		}
		b[i] = letters[rint.Int64()]
	}
	return string(b), nil
}
