package main

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"

	"github.com/awgh/sshell"
)

type ESCfg struct {
	Port     int
	User     string
	Password string
}

func main() {

	//read the cfg file
	cfg := ESCfg{2022, "easync", "easync.2021"}

	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		log.Println(err)
	}
	new_path := filepath.Join(dir, "cfg.json")
	data, err := ioutil.ReadFile(new_path)
	if err == nil {
		err = json.Unmarshal([]byte(data), &cfg)
		if err != nil {
			log.Println(err)

		}
	}

	shell := sshell.SSHell{User: cfg.User, Password: cfg.Password, Port: cfg.Port, Prompt: "> "}
	shell.Listen()

}

