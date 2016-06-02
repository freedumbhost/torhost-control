package main

import (
	"fmt"
	"github.com/gorilla/securecookie"
	"io/ioutil"
	"os"
)

func main() {
	// Check if we need to run
	_, err := os.Stat("config/")
	if err == nil {
		fmt.Printf("Configuration already exists (probably -- aborting): %v\r\n", err)
		return
	}

	// Create configuration directory
	err = os.Mkdir("config/", 0700) // readable only by *us*
	if err != nil {
		fmt.Printf("Failed to create configuration directory: %v\r\n", err)
		return
	}

	// Generate keys for storing our session
	authKey := securecookie.GenerateRandomKey(32)
	encKey := securecookie.GenerateRandomKey(32)

	// Write our keys to a file
	err = ioutil.WriteFile("config/auth.key", authKey, 0600)
	if err != nil {
		fmt.Printf("Failed to create auth.key: %v\r\n", err)
		return
	}
	err = ioutil.WriteFile("config/enc.key", encKey, 0600)
	if err != nil {
		fmt.Printf("Failed to create enc.key: %v\r\n", err)
		return
	}

	// Configuration complete!
	fmt.Println("Configuration complete")
}
