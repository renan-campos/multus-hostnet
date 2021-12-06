package main

import (
	"fmt"
	"os"

	"github.com/vishvananda/netlink"
)

func main() {
	iface, found := os.LookupEnv("MIGRATED_IFACE")
	if !found {
		fmt.Println("MIGRATED_IFACE environment variable not found")
		os.Exit(1)
	}

	link, err := netlink.LinkByName(iface)
	if err != nil {
		panic(err.Error())
	}

	err = netlink.LinkDel(link)
	if err != nil {
		panic(err.Error())
	}
}
