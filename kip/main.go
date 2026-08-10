package main

import "github.com/getkipper/kipper/kip/cmd"

var version = "dev"

func main() {
	cmd.SetVersion(version)
	cmd.Execute()
}
