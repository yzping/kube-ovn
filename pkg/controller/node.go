package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"

	kubeovnv1 "github.com/kubeovn/kube-ovn/pkg/apis/kubeovn/v1"
	"github.com/kubeovn/kube-ovn/pkg/ovs"
	"github.com/kubeovn/kube-ovn/pkg/util"
	goping "github.com/oilbeater/go-ping"
)

func (c *Controller) enqueueAddNode(obj interface{}) {
	if !c.isLeader() {
		return
	}
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	klog.V(3).Infof("enqueue add node %s", key)
	c.addNodeQueue.Add(key)
}

func nodeReady(node *v1.Node) bool {
	for _, con := range node.Status.Conditions {
		if con.Type == v1.NodeReady && con.Status == v1.ConditionTrue {
			return true
		}
	}
	return false
}

func (c *Controller) enqueueUpdateNode(oldObj, newObj interface{}) {
	if !c.isLeader() {
		return
	}

	oldNode := oldObj.(*v1.Node)
	newNode := newObj.(*v1.Node)

	if nodeReady(oldNode) != nodeReady(newNode) ||
		oldNode.Annotations[util.ChassisAnnotation] != newNode.Annotations[util.ChassisAnnotation] {
		var key string
		var err error
		if key, err = cache.MetaNamespaceKeyFunc(newObj); err != nil {
			utilruntime.HandleError(err)
			return
		}
		klog.V(3).Infof("enqueue update node %s", key)
		c.updateNodeQueue.Add(key)
	}
}

func (c *Controller) enqueueDeleteNode(obj interface{}) {
	if !c.isLeader() {
		return
	}
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	klog.V(3).Infof("enqueue delete node %s", key)
	c.deleteNodeQueue.Add(key)
}

func (c *Controller) runAddNodeWorker() {
	for c.processNextAddNodeWorkItem() {
	}
}

func (c *Controller) runUpdateNodeWorker() {
	for c.processNextUpdateNodeWorkItem() {
	}
}

func (c *Controller) runDeleteNodeWorker() {
	for c.processNextDeleteNodeWorkItem() {
	}
}

func (c *Controller) processNextAddNodeWorkItem() bool {
	obj, shutdown := c.addNodeQueue.Get()

	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.addNodeQueue.Done(obj)
		var key string
		var ok bool
		if key, ok = obj.(string); !ok {
			c.addNodeQueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		if err := c.handleAddNode(key); err != nil {
			c.addNodeQueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		c.addNodeQueue.Forget(obj)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}
	return true
}

func (c *Controller) processNextUpdateNodeWorkItem() bool {
	obj, shutdown := c.updateNodeQueue.Get()

	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.updateNodeQueue.Done(obj)
		var key string
		var ok bool
		if key, ok = obj.(string); !ok {
			c.updateNodeQueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		if err := c.handleUpdateNode(key); err != nil {
			c.updateNodeQueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		c.updateNodeQueue.Forget(obj)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}
	return true
}

func (c *Controller) processNextDeleteNodeWorkItem() bool {
	obj, shutdown := c.deleteNodeQueue.Get()

	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.deleteNodeQueue.Done(obj)
		var key string
		var ok bool
		if key, ok = obj.(string); !ok {
			c.deleteNodeQueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		if err := c.handleDeleteNode(key); err != nil {
			c.deleteNodeQueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		c.deleteNodeQueue.Forget(obj)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}
	return true
}

func (c *Controller) handleAddNode(key string) error {
	node, err := c.nodesLister.Get(key)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	subnets, err := c.subnetsLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list subnets: %v", err)
		return err
	}

	nodeIP := util.GetNodeInternalIP(*node)
	for _, subnet := range subnets {
		if subnet.Spec.Vlan == "" && subnet.Spec.Vpc == util.DefaultVpc && util.CIDRContainIP(subnet.Spec.CIDRBlock, nodeIP) {
			msg := fmt.Sprintf("internal IP address of node %s is in CIDR of subnet %s, this may result in network issues", node.Name, subnet.Name)
			klog.Warning(msg)
			c.recorder.Eventf(&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: node.Name, UID: types.UID(node.Name)}}, v1.EventTypeWarning, "NodeAddressConflictWithSubnet", msg)
			break
		}
	}

	providerNetworks, err := c.providerNetworksLister.List(labels.Everything())
	if err != nil && !k8serrors.IsNotFound(err) {
		klog.Errorf("failed to list provider networks: %v", err)
		return err
	}
	for _, pn := range providerNetworks {
		if !util.ContainsString(pn.Spec.ExcludeNodes, node.Name) {
			if pn.Status.EnsureNodeStandardConditions(key) {
				bytes, err := pn.Status.Bytes()
				if err != nil {
					klog.Error(err)
					return err
				}
				_, err = c.config.KubeOvnClient.KubeovnV1().ProviderNetworks().Patch(context.Background(), pn.Name, types.MergePatchType, bytes, metav1.PatchOptions{})
				if err != nil {
					klog.Errorf("failed to patch provider network %s: %v", pn.Name, err)
					return err
				}
			}
		}
	}

	subnet, err := c.subnetsLister.Get(c.config.NodeSwitch)
	if err != nil {
		klog.Errorf("failed to get node subnet %v", err)
		return err
	}

	if err := c.retryDelDupChassis(util.ChasRetryTime, util.ChasRetryIntev+2, c.checkChassisDupl, node); err != nil {
		return err
	}

	var v4IP, v6IP, mac string
	portName := fmt.Sprintf("node-%s", key)
	if node.Annotations[util.AllocatedAnnotation] == "true" && node.Annotations[util.IpAddressAnnotation] != "" && node.Annotations[util.MacAddressAnnotation] != "" {
		v4IP, v6IP, mac, err = c.ipam.GetStaticAddress(portName, node.Annotations[util.IpAddressAnnotation],
			node.Annotations[util.MacAddressAnnotation],
			node.Annotations[util.LogicalSwitchAnnotation])
		if err != nil {
			klog.Errorf("failed to alloc static ip addrs for node %v, err %v", node.Name, err)
			return err
		}
	} else {
		v4IP, v6IP, mac, err = c.ipam.GetRandomAddress(portName, c.config.NodeSwitch, nil)
		if err != nil {
			klog.Errorf("failed to alloc random ip addrs for node %v, err %v", node.Name, err)
			return err
		}
	}

	ipStr := util.GetStringIP(v4IP, v6IP)
	if err := c.ovnClient.CreatePort(c.config.NodeSwitch, portName, ipStr, subnet.Spec.CIDRBlock, mac, "", "", "", false); err != nil {
		return err
	}

	// There is only one nodeAddr temp
	nodeAddr := util.GetNodeInternalIP(*node)
	for _, ip := range strings.Split(ipStr, ",") {
		if util.CheckProtocol(nodeAddr) == util.CheckProtocol(ip) {
			err = c.ovnClient.AddStaticRoute("", nodeAddr, ip, c.config.ClusterRouter, util.NormalRouteType)
			if err != nil {
				klog.Errorf("failed to add static router from node to ovn0 %v", err)
				return err
			}
		}
	}

	patchPayloadTemplate :=
		`[{
        "op": "%s",
        "path": "/metadata/annotations",
        "value": %s
    }]`
	op := "replace"
	if len(node.Annotations) == 0 {
		node.Annotations = map[string]string{}
		op = "add"
	}

	node.Annotations[util.IpAddressAnnotation] = ipStr
	node.Annotations[util.MacAddressAnnotation] = mac
	node.Annotations[util.CidrAnnotation] = subnet.Spec.CIDRBlock
	node.Annotations[util.GatewayAnnotation] = subnet.Spec.Gateway
	node.Annotations[util.LogicalSwitchAnnotation] = c.config.NodeSwitch
	node.Annotations[util.AllocatedAnnotation] = "true"
	node.Annotations[util.PortNameAnnotation] = fmt.Sprintf("node-%s", key)
	raw, _ := json.Marshal(node.Annotations)
	patchPayload := fmt.Sprintf(patchPayloadTemplate, op, raw)
	_, err = c.config.KubeClient.CoreV1().Nodes().Patch(context.Background(), key, types.JSONPatchType, []byte(patchPayload), metav1.PatchOptions{}, "")
	if err != nil {
		klog.Errorf("patch node %s failed %v", key, err)
		return err
	}

	if err := c.createOrUpdateCrdIPs(key, ipStr, mac); err != nil {
		klog.Errorf("failed to create or update IPs node-%s %v", key, err)
		return err
	}

	return nil
}

func (c *Controller) handleDeleteNode(key string) error {
	portName := fmt.Sprintf("node-%s", key)
	if err := c.ovnClient.DeleteLogicalSwitchPort(portName); err != nil {
		klog.Errorf("failed to delete node switch port node-%s %v", key, err)
		return err
	}
	if err := c.ovnClient.DeleteChassis(key); err != nil {
		klog.Errorf("failed to delete chassis for node %s %v", key, err)
		return err
	}

	if err := c.config.KubeOvnClient.KubeovnV1().IPs().Delete(context.Background(), portName, metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
		return err
	}

	addresses := c.ipam.GetPodAddress(portName)
	for _, addr := range addresses {
		if err := c.ovnClient.DeleteStaticRouteByNextHop(addr.Ip); err != nil {
			return err
		}
	}

	c.ipam.ReleaseAddressByPod(portName)

	providerNetworks, err := c.providerNetworksLister.List(labels.Everything())
	if err != nil && !k8serrors.IsNotFound(err) {
		klog.Errorf("failed to list provider networks: %v", err)
		return err
	}

	for _, pn := range providerNetworks {
		if err = c.updateProviderNetworkStatusForNodeDeletion(pn, key); err != nil {
			return err
		}
	}

	return nil
}

func (c *Controller) updateProviderNetworkStatusForNodeDeletion(pn *kubeovnv1.ProviderNetwork, node string) error {
	if util.ContainsString(pn.Status.ReadyNodes, node) {
		pn.Status.ReadyNodes = util.RemoveString(pn.Status.ReadyNodes, node)
		if len(pn.Status.ReadyNodes) == 0 {
			bytes := []byte(`[{ "op": "remove", "path": "/status/readyNodes"}]`)
			_, err := c.config.KubeOvnClient.KubeovnV1().ProviderNetworks().Patch(context.Background(), pn.Name, types.JSONPatchType, bytes, metav1.PatchOptions{})
			if err != nil {
				klog.Errorf("failed to patch provider network %s: %v", pn.Name, err)
				return err
			}
		} else {
			bytes, err := pn.Status.Bytes()
			if err != nil {
				klog.Error(err)
				return err
			}
			_, err = c.config.KubeOvnClient.KubeovnV1().ProviderNetworks().Patch(context.Background(), pn.Name, types.MergePatchType, bytes, metav1.PatchOptions{})
			if err != nil {
				klog.Errorf("failed to patch provider network %s: %v", pn.Name, err)
				return err
			}
		}
	}
	if pn.Status.RemoveNodeConditions(node) {
		if len(pn.Status.Conditions) == 0 {
			bytes := []byte(`[{ "op": "remove", "path": "/status/conditions"}]`)
			_, err := c.config.KubeOvnClient.KubeovnV1().ProviderNetworks().Patch(context.Background(), pn.Name, types.JSONPatchType, bytes, metav1.PatchOptions{})
			if err != nil {
				klog.Errorf("failed to patch provider network %s: %v", pn.Name, err)
				return err
			}
		} else {
			bytes, err := pn.Status.Bytes()
			if err != nil {
				klog.Error(err)
				return err
			}
			_, err = c.config.KubeOvnClient.KubeovnV1().ProviderNetworks().Patch(context.Background(), pn.Name, types.MergePatchType, bytes, metav1.PatchOptions{})
			if err != nil {
				klog.Errorf("failed to patch provider network %s: %v", pn.Name, err)
				return err
			}
		}
	}

	return nil
}

func (c *Controller) handleUpdateNode(key string) error {
	node, err := c.nodesLister.Get(key)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	subnets, err := c.subnetsLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to get subnets %v", err)
		return err
	}

	if err := c.retryDelDupChassis(util.ChasRetryTime, util.ChasRetryIntev+2, c.checkChassisDupl, node); err != nil {
		return err
	}

	for _, subnet := range subnets {
		if util.GatewayContains(subnet.Spec.GatewayNode, node.Name) {
			if err := c.reconcileGateway(subnet); err != nil {
				return err
			}
		}
	}

	return nil
}

func (c *Controller) createOrUpdateCrdIPs(key, ip, mac string) error {
	v4IP, v6IP := util.SplitStringIP(ip)
	ipCr, err := c.config.KubeOvnClient.KubeovnV1().IPs().Get(context.Background(), fmt.Sprintf("node-%s", key), metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			_, err := c.config.KubeOvnClient.KubeovnV1().IPs().Create(context.Background(), &kubeovnv1.IP{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("node-%s", key),
					Labels: map[string]string{
						util.SubnetNameLabel: c.config.NodeSwitch,
						c.config.NodeSwitch:  "",
					},
				},
				Spec: kubeovnv1.IPSpec{
					PodName:       key,
					Subnet:        c.config.NodeSwitch,
					NodeName:      key,
					IPAddress:     ip,
					V4IPAddress:   v4IP,
					V6IPAddress:   v6IP,
					MacAddress:    mac,
					AttachIPs:     []string{},
					AttachMacs:    []string{},
					AttachSubnets: []string{},
				},
			}, metav1.CreateOptions{})
			if err != nil {
				errMsg := fmt.Errorf("failed to create ip crd for %s, %v", ip, err)
				klog.Error(errMsg)
				return errMsg
			}
		} else {
			errMsg := fmt.Errorf("failed to get ip crd for %s, %v", ip, err)
			klog.Error(errMsg)
			return errMsg
		}
	} else {
		if ipCr.Labels != nil {
			ipCr.Labels[util.SubnetNameLabel] = c.config.NodeSwitch
		} else {
			ipCr.Labels = map[string]string{
				util.SubnetNameLabel: c.config.NodeSwitch,
			}
		}
		ipCr.Spec.PodName = key
		ipCr.Spec.Namespace = ""
		ipCr.Spec.Subnet = c.config.NodeSwitch
		ipCr.Spec.NodeName = key
		ipCr.Spec.IPAddress = ip
		ipCr.Spec.V4IPAddress = v4IP
		ipCr.Spec.V6IPAddress = v6IP
		ipCr.Spec.MacAddress = mac
		ipCr.Spec.ContainerID = ""
		_, err := c.config.KubeOvnClient.KubeovnV1().IPs().Update(context.Background(), ipCr, metav1.UpdateOptions{})
		if err != nil {
			errMsg := fmt.Errorf("failed to create ip crd for %s, %v", ip, err)
			klog.Error(errMsg)
			return errMsg
		}
	}

	return nil
}

func (c *Controller) CheckGatewayReady() {
	if err := c.checkGatewayReady(); err != nil {
		klog.Errorf("failed to check gateway ready %v", err)
	}
}

func (c *Controller) checkGatewayReady() error {
	klog.V(3).Infoln("start to check gateway status")
	subnetList, err := c.subnetsLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list subnets %v", err)
		return err
	}
	nodes, err := c.nodesLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list nodes, %v", err)
		return err
	}

	for _, subnet := range subnetList {
		if subnet.Spec.UnderlayGateway || subnet.Spec.GatewayType != kubeovnv1.GWCentralizedType || subnet.Spec.GatewayNode == "" {
			continue
		}

		for _, node := range nodes {
			ipStr := node.Annotations[util.IpAddressAnnotation]
			for _, ip := range strings.Split(ipStr, ",") {
				var cidrBlock string
				for _, cidrBlock = range strings.Split(subnet.Spec.CIDRBlock, ",") {
					if util.CheckProtocol(cidrBlock) != util.CheckProtocol(ip) {
						continue
					}
				}

				exist, err := c.checkNodeEcmpRouteExist(ip, cidrBlock)
				if err != nil {
					klog.Errorf("get ecmp static route for subnet %v, error %v", subnet.Name, err)
					break
				}

				if util.GatewayContains(subnet.Spec.GatewayNode, node.Name) {
					pinger, err := goping.NewPinger(ip)
					if err != nil {
						return fmt.Errorf("failed to init pinger, %v", err)
					}
					pinger.SetPrivileged(true)

					count := 5
					pinger.Count = count
					pinger.Timeout = time.Duration(count) * time.Second
					pinger.Interval = 1 * time.Second

					success := false
					pinger.OnRecv = func(p *goping.Packet) {
						success = true
						pinger.Stop()
					}
					pinger.Run()

					if !nodeReady(node) {
						success = false
					}

					if !success {
						klog.Warningf("failed to ping ovn0 %s or node %v is not ready", ip, node.Name)
						if exist {
							if err := c.ovnClient.DeleteMatchedStaticRoute(cidrBlock, ip, c.config.ClusterRouter); err != nil {
								klog.Errorf("failed to delete static route %s for node %s, %v", ip, node.Name, err)
								return err
							}
						}
					} else {
						klog.V(3).Infof("succeed to ping gw %s", ip)
						if !exist {
							if err := c.ovnClient.AddStaticRoute(ovs.PolicySrcIP, subnet.Spec.CIDRBlock, ip, c.config.ClusterRouter, util.EcmpRouteType); err != nil {
								klog.Errorf("failed to add static route for node %s, %v", node.Name, err)
								return err
							}
						}
					}
				} else {
					if exist {
						klog.Infof("subnet %v gatewayNode does not contains node %v, should delete ecmp route for node ip %s", subnet.Name, node.Name, ip)
						if err := c.ovnClient.DeleteMatchedStaticRoute(cidrBlock, ip, c.config.ClusterRouter); err != nil {
							klog.Errorf("failed to delete static route %s for node %s, %v", ip, node.Name, err)
							return err
						}
					}
				}
			}
		}
	}
	return nil
}

func (c *Controller) checkNodeEcmpRouteExist(nodeIp, cidrBlock string) (bool, error) {
	routes, err := c.ovnClient.GetStaticRouteList(c.config.ClusterRouter)
	if err != nil {
		klog.Errorf("failed to list static route %v", err)
		return false, err
	}

	for _, route := range routes {
		if route.Policy != ovs.PolicySrcIP {
			continue
		}
		if route.CIDR == cidrBlock && route.NextHop == nodeIp {
			klog.V(3).Infof("src-ip static route exist for cidr %s, nexthop %v", cidrBlock, nodeIp)
			return true, nil
		}
	}
	return false, nil
}

func (c *Controller) checkChassisDupl(node *v1.Node) error {
	// notice that multiple chassises may arise and we are not prepared
	chassisAdd, err := c.ovnClient.GetChassis(node.Name)
	if err != nil {
		klog.Errorf("failed to get node %s chassisID, %v", node.Name, err)
		return err
	}
	chassisAnn := node.Annotations[util.ChassisAnnotation]
	if chassisAnn == chassisAdd || chassisAnn == "" {
		return nil
	}

	klog.Errorf("duplicate chassis for node %s and new chassis %s", node.Name, chassisAdd)
	if err := c.ovnClient.DeleteChassis(node.Name); err != nil {
		klog.Errorf("failed to delete chassis for node %s %v", node.Name, err)
		return err
	}
	return errors.New("deleting dismatch chassis id")
}

func (c *Controller) retryDelDupChassis(attempts int, sleep int, f func(node *v1.Node) error, node *v1.Node) (err error) {
	i := 0
	for ; ; i++ {
		err = f(node)
		if err == nil {
			return
		}
		if i >= (attempts - 1) {
			break
		}
		time.Sleep(time.Duration(sleep) * time.Second)
	}
	if i >= (attempts - 1) {
		errMsg := fmt.Errorf("exhausting all attempts")
		klog.Error(errMsg)
		return errMsg
	}
	klog.V(3).Infof("finish check chassis")
	return nil
}
