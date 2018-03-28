/*
Copyright 2018 The Kubernetes Authors.
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

package main

import (
	"github.com/golang/glog"
	"github.com/spf13/pflag"

	"k8s.io/apiserver/pkg/util/logs"
	"k8s.io/client-go/kubernetes"

	"github.com/kubernetes-incubator/apiserver-builder/pkg/controller"

	"k8s.io/kube-deploy/cluster-api/pkg/client/clientset_generated/clientset"
	"k8s.io/kube-deploy/cluster-api/pkg/controller/config"
	"k8s.io/kube-deploy/cluster-api/pkg/controller/machine"
	"k8s.io/kube-deploy/cluster-api/pkg/controller/sharedinformers"

	"github.com/csrwng/aws-machine-controller/pkg/aws"
)

func init() {
	config.ControllerConfig.AddFlags(pflag.CommandLine)
}

func main() {
	pflag.Parse()

	logs.InitLogs()
	defer logs.FlushLogs()

	config, err := controller.GetConfig(config.ControllerConfig.Kubeconfig)
	if err != nil {
		glog.Fatalf("Could not create Config for talking to the apiserver: %v", err)
	}

	client, err := clientset.NewForConfig(config)
	if err != nil {
		glog.Fatalf("Could not create client for talking to the apiserver: %v", err)
	}

	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		glog.Fatalf("Could not create kubernetes client to talk to the apiserver: %v", err)
	}

	actuator := aws.NewActuator(kubeClient, client)
	shutdown := make(chan struct{})
	si := sharedinformers.NewSharedInformers(config, shutdown)
	c := machine.NewMachineController(config, si, actuator)
	c.Run(shutdown)
	select {}
}
