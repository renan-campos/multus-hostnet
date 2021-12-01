package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"text/template"

	batch "k8s.io/api/batch/v1"

	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	//go:embed template/setup-job.yaml
	setupJobTemplate string

	//go:embed template/teardown-job.yaml
	teardownJobTemplate string
)

func main() {

	fmt.Println("Setting up SIGTERM handler")
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGTERM)

	fmt.Println("Setting up k8s client")
	k8sClient, err := setupK8sClient()
	if err != nil {
		panic(err.Error())
	}

	fmt.Println("Finding myself")
	self := findMyself(k8sClient)

	fmt.Println("Running setup job")
	runSetupJob(k8sClient, self.NodeName, self.Namespace)

	fmt.Println("Waiting for SIGTERM signal")
	<-signalChan

	fmt.Println("Running teardown job")
	runTeardownJob(k8sClient, self.NodeName, self.Namespace)
}

func runSetupJob(clientset *kubernetes.Clientset, nodeName, namespace string) {
	pJob, err := templateToJob("setup-job", setupJobTemplate, templateParam{
		NodeName:  nodeName,
		Namespace: namespace,
	})
	if err != nil {
		panic(err.Error())
	}

	err = runReplaceableJob(context.TODO(), clientset, pJob)
	if err != nil {
		panic(err.Error())
	}
}

func runTeardownJob(clientset *kubernetes.Clientset, nodeName, namespace string) {
	pJob, err := templateToJob("teardown-job", teardownJobTemplate, templateParam{
		NodeName:  nodeName,
		Namespace: namespace,
	})
	if err != nil {
		panic(err.Error())
	}

	err = runReplaceableJob(context.TODO(), clientset, pJob)
	if err != nil {
		panic(err.Error())
	}
}

type selfData struct {
	NodeName  string
	Namespace string
}

func findMyself(clientset *kubernetes.Clientset) selfData {
	// Fetching the pods with the appropriate daemonset label
	pods, err := clientset.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{
		LabelSelector: "app=multus-hostnet",
	})
	if err != nil {
		panic(err.Error())
	}

	interfaces, err := net.Interfaces()
	if err != nil {
		panic(err.Error())
	}
	for _, pod := range pods.Items {
		_, err := findInterface(interfaces, pod.Status.PodIP)
		if err != nil && err != InterfaceNotFound {
			panic(err.Error())
		}
		if err == InterfaceNotFound {
			continue
		} else {
			return selfData{
				NodeName:  pod.Spec.NodeName,
				Namespace: pod.ObjectMeta.Namespace,
			}
		}
	}
	panic(errors.New("Could not find myself... next time try yoga"))
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

var InterfaceNotFound = errors.New("Interface with matching IP not found")

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

	return ifaceName, InterfaceNotFound
}

type templateParam struct {
	NodeName  string
	Namespace string
}

func templateToJob(name, templateData string, p templateParam) (*batch.Job, error) {
	var job batch.Job
	t, err := loadTemplate(name, templateData, p)
	if err != nil {
		return nil, errors.Wrap(err, "failed to load job template")
	}

	err = yaml.Unmarshal([]byte(t), &job)
	if err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal job template")
	}
	return &job, nil
}

func loadTemplate(name, templateData string, p templateParam) ([]byte, error) {
	var writer bytes.Buffer
	t := template.New(name)
	t, err := t.Parse(templateData)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse template %v", name)
	}
	err = t.Execute(&writer, p)
	return writer.Bytes(), err
}

func runReplaceableJob(ctx context.Context, clientset kubernetes.Interface, job *batch.Job) error {
	// check if the job was already created and what its status is
	existingJob, err := clientset.BatchV1().Jobs(job.Namespace).Get(ctx, job.Name, metav1.GetOptions{})
	if err != nil && !k8sErrors.IsNotFound(err) {
		return err
	} else if err == nil {
		// delete the job that already exists from a previous run
		err := clientset.BatchV1().Jobs(existingJob.Namespace).Delete(ctx, existingJob.Name, metav1.DeleteOptions{})
		if err != nil {
			return fmt.Errorf("failed to remove job %s. %+v", job.Name, err)
		}
	}

	_, err = clientset.BatchV1().Jobs(job.Namespace).Create(ctx, job, metav1.CreateOptions{})
	return err
}
