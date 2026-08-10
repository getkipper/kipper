package main

import "github.com/getkipper/kipper/datamover/cmd"

var version = "dev"

func main() {
	cmd.SetVersion(version)
	cmd.Execute()
}
