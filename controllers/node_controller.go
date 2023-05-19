/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	ma "github.com/multiformats/go-multiaddr"
	"github.com/waku-org/go-waku/waku/v2/rpc"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	wakuv1alpha1 "github.com/vpavlin/waku-operator/api/v1alpha1"
)

// NodeReconciler reconciles a Node object
type NodeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=waku.vac.dev,resources=nodes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=waku.vac.dev,resources=nodes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=waku.vac.dev,resources=nodes/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=pods;services,verbs=get;list;watch;create;update;patch;delete;

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Node object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	node := &wakuv1alpha1.Node{}
	err := r.Get(ctx, req.NamespacedName, node)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Log.Info("Node object not found, probably deleted")
			return ctrl.Result{}, nil
		}
		log.Log.Error(err, "Failed to get node object")
		return ctrl.Result{}, err
	}

	foundPod := &v1.Pod{}
	err = r.Get(ctx, types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, foundPod)
	if err != nil && errors.IsNotFound(err) {
		pod := r.podFromNode(ctx, node)
		err = r.Create(ctx, pod)
		if err != nil {
			log.Log.Error(err, "Failed to create corresponding pod", "Node.Namespace", node.Namespace, "Node.Name", node.Name)
			return ctrl.Result{}, err
		}

		return ctrl.Result{Requeue: true}, nil
	} else if err != nil {
		log.Log.Error(err, "Failed to get pod")
		return ctrl.Result{}, err
	}

	foundService := &v1.Service{}
	err = r.Get(ctx, types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, foundService)
	if err != nil && errors.IsNotFound(err) {
		service := r.serviceFromNode(node)
		err = r.Create(ctx, service)
		if err != nil {
			log.Log.Error(err, "Failed to create corresponding pod", "Pod.Namespace", service.Namespace, "Pod.Name", service.Name)
			return ctrl.Result{}, err
		}

		return ctrl.Result{Requeue: true}, nil
	} else if err != nil {
		log.Log.Error(err, "Failed to get pod")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&wakuv1alpha1.Node{}).
		Owns(&v1.Pod{}).
		Complete(r)
}

func (r *NodeReconciler) serviceFromNode(node *wakuv1alpha1.Node) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: node.Namespace,
		},
		Spec: v1.ServiceSpec{
			Selector: map[string]string{
				"app":  "wakunode",
				"node": node.Name,
			},
			Ports: []v1.ServicePort{
				{
					Name: "libp2p",
					Port: 60000,
				},
				{
					Name: "rpc",
					Port: 8545,
				},
			},
		},
	}
}

func (r *NodeReconciler) podFromNode(ctx context.Context, node *wakuv1alpha1.Node) *v1.Pod {
	command := []string{}

	command = append(command, "--rpc=true", "--rpc-address=0.0.0.0")

	command = append(command, fmt.Sprintf("--dns4-domain-name=%s", node.Name))

	if node.Spec.Metrics {
		command = append(command, "--metrics-server=True", "--metrics-server-address=0.0.0.0")
	}

	if node.Spec.Discv5.Discovery {
		command = append(command, "--discv5-discovery=true")
	}

	if node.Spec.Discv5.EnrAutoUpdate {
		command = append(command, "--discv5-enr-auto-update=True")
	}

	if len(node.Spec.Discv5.BootstrapNode) > 0 {
		command = append(command, fmt.Sprintf("--discv5-bootstrap-node=%s", node.Spec.Discv5.BootstrapNode))
	}

	if node.Spec.Discv5.UdpPort != 0 {
		command = append(command, fmt.Sprintf("--discv5-udp-port=%d", node.Spec.Discv5.UdpPort))
	}

	for _, p := range node.Spec.Protocols {
		switch p {
		case "relay":
			command = append(command, "--relay=true")
		case "store":
			command = append(command, "--store=true")
		case "lightpush":
			command = append(command, "--lightpush=true")
		}
	}

	if len(node.Spec.StaticNode) != 0 {
		m := node.Spec.StaticNode
		_, err := ma.NewMultiaddr(node.Spec.StaticNode)
		if err != nil {
			srv := &v1.Service{}
			err = r.Get(ctx, types.NamespacedName{Name: node.Spec.StaticNode, Namespace: node.Namespace}, srv)
			if err != nil {
				return nil
			}

			ip := srv.Spec.ClusterIP
			port := 8545

			args := rpc.InfoArgs{}
			data, err := json.Marshal(args)
			if err != nil {
				return nil
			}

			resp, err := http.Post(fmt.Sprintf("http://%s:%d", ip, port), "application/json", bytes.NewBuffer(data))
			if err != nil {
				fmt.Println(err)
				return nil
			}

			defer resp.Body.Close()
			body, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				return nil
			}

			fmt.Println(body)

			reply := &rpc.InfoReply{}
			err = json.Unmarshal(body, reply)
			if err != nil {
				return nil
			}

			fmt.Println(body)
			m = reply.ListenAddresses[0]
		}
		command = append(command, "--staticnode=%s", m)
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: node.Namespace,
			Labels: map[string]string{
				"app":  "wakunode",
				"node": node.Name,
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "wakunode",
					Image: node.Spec.Image,
					Args:  command,
					Ports: []v1.ContainerPort{
						{
							Name:          "rpc",
							ContainerPort: 8548,
						},
						{
							Name:          "libp2p",
							ContainerPort: 60000,
						},
					},
				},
			},
		},
	}

	return pod
}
