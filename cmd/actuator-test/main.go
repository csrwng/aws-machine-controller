package main

import (
	"bytes"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	jsonserializer "k8s.io/apimachinery/pkg/runtime/serializer/json"

	coapi "github.com/openshift/cluster-operator/pkg/api"
	cov1 "github.com/openshift/cluster-operator/pkg/apis/clusteroperator/v1alpha1"

	clusterv1 "k8s.io/kube-deploy/cluster-api/pkg/apis/cluster/v1alpha1"

	"github.com/csrwng/aws-machine-controller/pkg/aws"
)

func usage() {
	fmt.Printf("Usage: %s CLUSTER-NAME\n\n", os.Args[0])
}

func strptr(str string) *string {
	return &str
}

func createClusterMachine(name string) {

	pullPolicy := corev1.PullAlways

	// cluster version
	coVersion := cov1.ClusterVersion{}
	coVersion.Name = "origin-v3-9"
	coVersion.Spec = cov1.ClusterVersionSpec{
		ImageFormat:    "openshift/origin-${component}:v3.9.0",
		DeploymentType: cov1.ClusterDeploymentTypeOrigin,
		VMImages: cov1.VMImages{
			AWSImages: &cov1.AWSVMImages{
				RegionAMIs: []cov1.AWSRegionAMIs{
					{
						Region: "us-east-1",
						AMI:    "ami-833d37f9",
					},
				},
			},
		},
		Version:                         "v3.9.0",
		OpenshiftAnsibleImage:           strptr("openshift/origin-ansible:v3.9.0"),
		OpenshiftAnsibleImagePullPolicy: &pullPolicy,
	}

	// cluster-operator cluster
	coCluster := cov1.Cluster{}
	coCluster.Name = name
	coClusterAWS := &cov1.AWSClusterSpec{}
	coClusterAWS.AccountSecret.Name = "aws-credentials"
	coClusterAWS.SSHSecret.Name = "ssh-private-key"
	coClusterAWS.SSHUser = "centos"
	coClusterAWS.SSLSecret.Name = "ssl-cert"
	coClusterAWS.Region = "us-east-1"
	coClusterAWS.KeyPairName = "libra"
	coCluster.Spec.Hardware.AWS = coClusterAWS
	coCluster.Spec.DefaultHardwareSpec = &cov1.MachineSetHardwareSpec{
		AWS: &cov1.MachineSetAWSHardwareSpec{
			InstanceType: "t2.xlarge",
		},
	}
	coCluster.Spec.MachineSets = []cov1.ClusterMachineSet{
		{
			ShortName: "master",
			MachineSetConfig: cov1.MachineSetConfig{
				NodeType: cov1.NodeTypeMaster,
				Size:     1,
			},
		},
		{
			ShortName: "infra",
			MachineSetConfig: cov1.MachineSetConfig{
				NodeType: cov1.NodeTypeCompute,
				Size:     1,
				Infra:    true,
			},
		},
		{
			ShortName: "compute",
			MachineSetConfig: cov1.MachineSetConfig{
				NodeType: cov1.NodeTypeCompute,
				Size:     1,
			},
		},
	}

	// cluster-operator machineset
	coMachineSet := cov1.MachineSet{}
	coMachineSet.Name = name + "-compute-1"
	coMachineSet.Spec.MachineSetConfig = cov1.MachineSetConfig{
		NodeType: cov1.NodeTypeCompute,
		Size:     1,
		Hardware: &cov1.MachineSetHardwareSpec{
			AWS: &cov1.MachineSetAWSHardwareSpec{
				InstanceType: "t2.xlarge",
			},
		},
	}

	// Serialize cluster version and add it as an annotation to the MachineSet
	coMachineSet.Annotations = map[string]string{
		"cluster-operator.openshift.io/cluster-version": serializeCOResource(&coVersion),
	}

	// Now define cluster-api resources
	cluster := clusterv1.Cluster{}
	cluster.Name = name
	cluster.Spec.ProviderConfig = serializeCOResource(&coCluster)

	machine := clusterv1.Machine{}
	machine.Name = name + "-compute-machine-1"
	machine.Spec.ProviderConfig.Value = &runtime.RawExtension{
		Raw: []byte(serializeCOResource(&coMachineSet)),
	}

	actuator := aws.NewActuator(nil, nil)
	_, err := actuator.CreateMachine(&cluster, &machine)
	if err != nil {
		fmt.Printf("Error occurred during machine creation: %v\n", err)
	} else {
		fmt.Printf("Machine creation was successful!")
	}
}

func serializeCOResource(object runtime.Object) string {
	serializer := jsonserializer.NewSerializer(jsonserializer.DefaultMetaFactory, coapi.Scheme, coapi.Scheme, false)
	encoder := coapi.Codecs.EncoderForVersion(serializer, cov1.SchemeGroupVersion)
	buffer := &bytes.Buffer{}
	encoder.Encode(object, buffer)
	return buffer.String()
}

func main() {
	if len(os.Args) != 2 {
		usage()
		return
	}

	clusterName := os.Args[1]

	createClusterMachine(clusterName)
}
