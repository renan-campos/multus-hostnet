package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	ifBase = "mlink"
	nsDir  = "/var/run/netns"
)

func main() {
	holderIP, found := os.LookupEnv("HOLDER_IP")
	if !found {
		fmt.Println("HOLDER_IP environment variable not found")
		os.Exit(1)
	}

	multusLinkName, found := os.LookupEnv("MULTUS_IFACE")
	if !found {
		fmt.Println("MULTUS_IFACE environment variable not found")
		os.Exit(1)
	}
	fmt.Printf("The multus interface is: %s\n", multusLinkName)

	controllerName, found := os.LookupEnv("CONTROLLER_NAME")
	if !found {
		fmt.Println("CONTROLLER_NAME environment variable not found")
		os.Exit(1)
	}
	controllerNamespace, found := os.LookupEnv("CONTROLLER_NAMESPACE")
	if !found {
		fmt.Println("CONTROLLER_NAMESPACE environment variable not found")
		os.Exit(1)
	}

	fmt.Println("Setting up k8s client")
	k8sClient, err := setupK8sClient()
	if err != nil {
		panic(err.Error())
	}

	holderNS, err := determineHolderNS(holderIP)
	if err != nil {
		panic(err.Error())
	}

	hostNS, err := ns.GetCurrentNS()
	if err != nil {
		panic(err.Error())
	}

	interfaces, err := net.Interfaces()
	if err != nil {
		panic(err.Error())
	}
	newLinkName, err := determineNewLinkName(interfaces)
	if err != nil {
		panic(err.Error())
	}

	err = annotateController(k8sClient, controllerName, controllerNamespace, newLinkName)
	if err != nil {
		panic(err.Error())
	}

	netConfig, err := determineMultusConfig(holderNS, multusLinkName)
	if err != nil {
		panic(err.Error())
	}

	err = migrateInterface(holderNS, hostNS, multusLinkName, newLinkName)
	if err != nil {
		panic(err.Error())
	}

	err = configureInterface(hostNS, newLinkName, netConfig)
	if err != nil {
		panic(err.Error())
	}
}

func determineHolderNS(holderIP string) (ns.NetNS, error) {
	var holderNS ns.NetNS

	nsFiles, err := ioutil.ReadDir(nsDir)
	if err != nil {
		return holderNS, errors.Wrap(err, "failed to read netns files")
	}

	for _, nsFile := range nsFiles {
		var foundNS bool

		tmpNS, err := ns.GetNS(filepath.Join(nsDir, nsFile.Name()))
		if err != nil {
			return holderNS, errors.Wrap(err, "failed to get network namespace")
		}

		err = tmpNS.Do(func(ns ns.NetNS) error {
			interfaces, err := net.Interfaces()
			if err != nil {
				return errors.Wrap(err, "failed to list interfaces")
			}

			iface, err := findInterface(interfaces, holderIP)
			if err != nil {
				return errors.Wrap(err, "failed to find needed interface")
			}
			if iface != "" {
				foundNS = true
				return nil
			}
			return nil
		})

		if err != nil {
			// Don't quit, just keep looking.
			fmt.Printf("failed to find holder network namespace: %v; continuing search\n", err)
			continue
		}

		if foundNS {
			holderNS = tmpNS
			return holderNS, nil
		}
	}

	return holderNS, errors.New("failed to find holder network namespace")
}

func determineNewLinkName(interfaces []net.Interface) (string, error) {
	var newLinkName string

	linkNumber := -1
	for _, iface := range interfaces {
		if idStrs := strings.Split(iface.Name, ifBase); len(idStrs) > 1 {
			id, err := strconv.Atoi(idStrs[1])
			if err != nil {
				return newLinkName, errors.Wrap(err, "failed to convert string to integer")
			}
			if id > linkNumber {
				linkNumber = id
			}
		}
	}
	linkNumber += 1

	newLinkName = fmt.Sprintf("%s%d", ifBase, linkNumber)
	fmt.Printf("new multus link name determined: %q\n", newLinkName)

	return newLinkName, nil
}

func findInterface(interfaces []net.Interface, ipStr string) (string, error) {
	var ifaceName string

	for _, iface := range interfaces {
		link, err := netlink.LinkByName(iface.Name)
		if err != nil {
			return ifaceName, errors.Wrap(err, "failed to get link")
		}
		if link == nil {
			return ifaceName, errors.New("failed to find link")
		}

		addrs, err := netlink.AddrList(link, 0)
		if err != nil {
			return ifaceName, errors.Wrap(err, "failed to get address from link")
		}

		for _, addr := range addrs {
			if addr.IP.String() == ipStr {
				linkAttrs := link.Attrs()
				if linkAttrs != nil {
					ifaceName = linkAttrs.Name
				}
				return ifaceName, nil
			}
		}
	}
	return ifaceName, nil
}

type multusConfig struct {
	Addrs  []netlink.Addr
	Routes []netlink.Route
}

func determineMultusConfig(holderNS ns.NetNS, multusLinkName string) (multusConfig, error) {
	var conf multusConfig

	err := holderNS.Do(func(ns ns.NetNS) error {
		link, err := netlink.LinkByName(multusLinkName)
		if err != nil {
			return errors.Wrap(err, "failed to get link")
		}

		conf.Addrs, err = netlink.AddrList(link, 0)
		if err != nil {
			return errors.Wrap(err, "failed to get address from link")
		}

		conf.Routes, err = netlink.RouteList(link, 0)
		if err != nil {
			return errors.Wrap(err, "failed to get routes from link")
		}

		return nil
	})

	if err != nil {
		return conf, errors.Wrap(err, "failed to get holder network namespace")
	}

	return conf, nil
}

func migrateInterface(holderNS, hostNS ns.NetNS, multusLinkName, newLinkName string) error {
	err := holderNS.Do(func(ns.NetNS) error {

		link, err := netlink.LinkByName(multusLinkName)
		if err != nil {
			return errors.Wrap(err, "failed to get multus link")
		}

		if err := netlink.LinkSetDown(link); err != nil {
			return errors.Wrap(err, "failed to set link down")
		}

		if err := netlink.LinkSetName(link, newLinkName); err != nil {
			return errors.Wrap(err, "failed to rename link")
		}

		// After renaming the link, the link object must be updated or netlink will get confused.
		link, err = netlink.LinkByName(newLinkName)
		if err != nil {
			return errors.Wrap(err, "failed to get link")
		}

		if err = netlink.LinkSetNsFd(link, int(hostNS.Fd())); err != nil {
			return errors.Wrap(err, "failed to move interface to host namespace")
		}

		return nil
	})

	if err != nil {
		return errors.Wrap(err, "failed to migrate multus interface")
	}
	return nil
}

func configureInterface(hostNS ns.NetNS, linkName string, conf multusConfig) error {
	err := hostNS.Do(func(ns.NetNS) error {
		link, err := netlink.LinkByName(linkName)
		if err != nil {
			return errors.Wrap(err, "failed to get interface on host namespace")
		}
		for _, addr := range conf.Addrs {
			// The IP address label must be changed to the new interface name
			// for the AddrAdd call to succeed.
			addr.Label = linkName
			if err := netlink.AddrAdd(link, &addr); err != nil {
				return errors.Wrap(err, "failed to configure ip address on interface")
			}
		}

		//for _, route := range conf.Routes {
		//	if err := netlink.RouteAdd(&route); err != nil {
		//		return errors.Wrap(err, "failed to configure route")
		//	}
		//}

		if err := netlink.LinkSetUp(link); err != nil {
			return errors.Wrap(err, "failed to set link up")
		}
		return nil
	})

	if err != nil {
		return errors.Wrap(err, "failed to configure multus interface on host namespace")
	}
	return nil
}

func setupK8sClient() (*kubernetes.Clientset, error) {
	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	// creates the clientset
	return kubernetes.NewForConfig(config)
}

func annotateController(k8sClient *kubernetes.Clientset, controllerName, controllerNamespace, migratedLinkName string) error {
	pod, err := k8sClient.CoreV1().Pods(controllerNamespace).Get(context.TODO(), controllerName, metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(err, "failed to get controller pod")
	}

	pod.ObjectMeta.Annotations["multus-migration"] = migratedLinkName

	_, err = k8sClient.CoreV1().Pods(controllerNamespace).Update(context.TODO(), pod, metav1.UpdateOptions{})
	if err != nil {
		return errors.Wrap(err, "failed to update controller pod")
	}

	return nil
}
