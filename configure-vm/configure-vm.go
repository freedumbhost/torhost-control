package main

import (
	"fmt"
	"regexp"
	"os"
	"os/exec"
	"strconv"
	"text/template"
	"io/ioutil"
	"bytes"
)

func main() {
	// Parse the VM ID out of /proc/cmdline to see if we need to run
	re := regexp.MustCompile(`\bvmid=([0-9]+)\b`)

	cmdlineBytes, err := ioutil.ReadFile("/proc/cmdline")
	if err != nil {
		fmt.Println("Unable to read /proc/cmdline. Closing")
		os.Exit(1)
	}

	cmdline := string(cmdlineBytes)

	matches := re.FindStringSubmatch(cmdline)

	if (matches == nil || len(matches) != 2) {
		fmt.Println("We're not running on a provisioned VM. Nothing to do. Closing")
		os.Exit(0);
	}

	vmid, err := strconv.Atoi(matches[1])
	if err != nil {
		fmt.Println("Invalid vmid")
		os.Exit(1)
	}

	if vmid < 1 || vmid > 254 {
		fmt.Println("Invalid vmid, outside of 1-254 range. vmid given:", vmid)
	}

	fmt.Println("Running for vmid:" , vmid)
	initVM(vmid)
}

func initVM(vmid int) {
	// Configure networking
	const network = `config_eth0="10.0.{{.Id}}.25/24"
routes_eth0="default via 10.0.{{.Id}}.5"
dns_servers_eth0="10.0.{{.Id}}.5"
`

	type VMInformation struct {
		Id int
	}

	vmInfo := VMInformation{vmid}

	t := template.Must(template.New("net").Parse(network))

	var net bytes.Buffer
	t.Execute(&net, vmInfo)

	err := ioutil.WriteFile("/etc/conf.d/net", net.Bytes(), 0644)
	if err != nil {
		fmt.Println("Error writing /etc/conf.d/net")
		os.Exit(2)
	}

	// Restart network
	err = exec.Command("/etc/init.d/net.eth0", "restart").Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}

	fmt.Println("Successfully initialized networking")
}
