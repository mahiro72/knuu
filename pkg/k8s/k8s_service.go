package k8s

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func (c *Client) GetService(ctx context.Context, name string) (*v1.Service, error) {
	svc, err := c.clientset.CoreV1().Services(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, ErrGettingService.WithParams(name).Wrap(err)
	}
	return svc, nil
}

func (c *Client) CreateService(
	ctx context.Context,
	name string,
	labels,
	selectorMap map[string]string,
	portsTCP,
	portsUDP []int,
) (*v1.Service, error) {
	svc, err := prepareService(c.namespace, name, labels, selectorMap, portsTCP, portsUDP)
	if err != nil {
		return nil, ErrPreparingService.WithParams(name).Wrap(err)
	}

	serv, err := c.clientset.CoreV1().Services(c.namespace).Create(ctx, svc, metav1.CreateOptions{})
	if err != nil {
		return nil, ErrCreatingService.WithParams(name).Wrap(err)
	}
	logrus.Debugf("Service %s created in namespace %s", name, c.namespace)
	return serv, nil
}

func (c *Client) PatchService(
	ctx context.Context,
	name string,
	labels,
	selectorMap map[string]string,
	portsTCP,
	portsUDP []int,
) (*v1.Service, error) {
	svc, err := prepareService(c.namespace, name, labels, selectorMap, portsTCP, portsUDP)
	if err != nil {
		return nil, ErrPreparingService.WithParams(name).Wrap(err)
	}

	serv, err := c.clientset.CoreV1().Services(c.namespace).Update(ctx, svc, metav1.UpdateOptions{})
	if err != nil {
		return nil, ErrPatchingService.WithParams(name).Wrap(err)
	}

	logrus.Debugf("Service %s patched in namespace %s", name, c.namespace)
	return serv, nil
}

func (c *Client) DeleteService(ctx context.Context, name string) error {
	_, err := c.GetService(ctx, name)
	if err != nil {
		return ErrGettingService.WithParams(name).Wrap(err)
	}

	err = c.clientset.CoreV1().Services(c.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return ErrDeletingService.WithParams(name).Wrap(err)
	}

	logrus.Debugf("Service %s deleted in namespace %s", name, c.namespace)
	return nil
}

func (c *Client) GetServiceIP(ctx context.Context, name string) (string, error) {
	svc, err := c.GetService(ctx, name)
	if err != nil {
		return "", ErrGettingService.WithParams(name).Wrap(err)
	}
	return svc.Spec.ClusterIP, nil
}

func buildPorts(tcpPorts, udpPorts []int) []v1.ServicePort {
	ports := make([]v1.ServicePort, 0, len(tcpPorts)+len(udpPorts))
	for _, port := range tcpPorts {
		ports = append(ports, v1.ServicePort{
			Name:       fmt.Sprintf("tcp-%d", port),
			Protocol:   v1.ProtocolTCP,
			Port:       int32(port),
			TargetPort: intstr.FromInt(port),
		})
	}
	for _, port := range udpPorts {
		ports = append(ports, v1.ServicePort{
			Name:       fmt.Sprintf("udp-%d", port),
			Protocol:   v1.ProtocolUDP,
			Port:       int32(port),
			TargetPort: intstr.FromInt(port),
		})
	}
	return ports
}

func prepareService(
	namespace, name string,
	labels, selectorMap map[string]string,
	tcpPorts, udpPorts []int,
) (*v1.Service, error) {
	if namespace == "" {
		return nil, ErrNamespaceRequired
	}
	if name == "" {
		return nil, ErrServiceNameRequired
	}
	if labels == nil {
		labels = make(map[string]string)
	}
	if selectorMap == nil {
		selectorMap = make(map[string]string)
	}

	servicePorts := buildPorts(tcpPorts, udpPorts)
	if len(servicePorts) == 0 {
		return nil, ErrNoPortsSpecified.WithParams(name)
	}

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels:    labels,
		},
		Spec: v1.ServiceSpec{
			Ports:    servicePorts,
			Selector: selectorMap,
			Type:     v1.ServiceTypeClusterIP,
		},
	}
	return svc, nil
}

func (c *Client) WaitForService(ctx context.Context, name string) error {
	ticker := time.NewTicker(waitRetry)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ErrTimeoutWaitingForServiceReady

		case <-ticker.C:
			ready, err := c.isServiceReady(ctx, name)
			if err != nil {
				return ErrCheckingServiceReady.WithParams(name).Wrap(err)
			}
			if !ready {
				continue
			}

			// Check if service is reachable
			endpoint, err := c.GetServiceEndpoint(ctx, name)
			if err != nil {
				return ErrGettingServiceEndpoint.WithParams(name).Wrap(err)
			}

			if err := checkServiceConnectivity(endpoint); err != nil {
				continue
			}

			// Service is reachable
			return nil
		}
	}
}

func (c *Client) GetServiceEndpoint(ctx context.Context, name string) (string, error) {
	srv, err := c.clientset.CoreV1().Services(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", ErrGettingService.WithParams(name).Wrap(err)
	}

	if srv.Spec.Type == v1.ServiceTypeLoadBalancer {
		// Use the LoadBalancer's external IP
		if len(srv.Status.LoadBalancer.Ingress) > 0 {
			return fmt.Sprintf("%s:%d", srv.Status.LoadBalancer.Ingress[0].IP, srv.Spec.Ports[0].Port), nil
		}
		return "", ErrLoadBalancerIPNotAvailable
	}

	if srv.Spec.Type == v1.ServiceTypeNodePort {
		// Use the Node IP and NodePort
		nodes, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return "", ErrGettingNodes.Wrap(err)
		}
		if len(nodes.Items) == 0 {
			return "", ErrNoNodesFound
		}

		// Use the first node for simplicity, you might need to handle multiple nodes
		var nodeIP string
		for _, address := range nodes.Items[0].Status.Addresses {
			if address.Type == "ExternalIP" {
				nodeIP = address.Address
				break
			}
		}
		return fmt.Sprintf("%s:%d", nodeIP, srv.Spec.Ports[0].NodePort), nil
	}

	return fmt.Sprintf("%s:%d", srv.Spec.ClusterIP, srv.Spec.Ports[0].Port), nil
}

func checkServiceConnectivity(serviceEndpoint string) error {
	conn, err := net.DialTimeout("tcp", serviceEndpoint, waitRetry)
	if err != nil {
		return ErrFailedToConnect.WithParams(serviceEndpoint).Wrap(err)
	}
	defer conn.Close()
	return nil // success
}

func (c *Client) isServiceReady(ctx context.Context, name string) (bool, error) {
	service, err := c.GetService(ctx, name)
	if err != nil {
		return false, ErrGettingService.WithParams(name).Wrap(err)
	}
	switch service.Spec.Type {
	case v1.ServiceTypeLoadBalancer:
		return len(service.Status.LoadBalancer.Ingress) > 0, nil
	case v1.ServiceTypeNodePort:
		return service.Spec.Ports[0].NodePort != 0, nil
	default:
		return len(service.Spec.ExternalIPs) > 0, nil
	}
}
