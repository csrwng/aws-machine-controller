package aws

import (
	"encoding/base64"
	"fmt"
	"io/ioutil"

	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes"
	clusterv1 "k8s.io/kube-deploy/cluster-api/pkg/apis/cluster/v1alpha1"
	clusterclient "k8s.io/kube-deploy/cluster-api/pkg/client/clientset_generated/clientset"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	coapi "github.com/openshift/cluster-operator/pkg/api"
	_ "github.com/openshift/cluster-operator/pkg/apis/clusteroperator"
	cov1 "github.com/openshift/cluster-operator/pkg/apis/clusteroperator/v1alpha1"
)

const (
	// Path to bootstrap kubeconfig. This needs to be mounted to the controller pod
	// as a secret when running this controller.
	bootstrapKubeConfig = "/etc/origin/master/bootstrap.kubeconfig"

	// For now, hardcode the default availability zone, we don't have one for
	// other regions
	defaultAvailabilityZone = "us-east-1c"

	// Hardcode IAM role for infra/compute for now
	defaultIAMRole = "openshift_node_describe_instances"

	// Annotation used to store serialized ClusterVersion resource in MachineSet
	clusterVersionAnnotation = "cluster-operator.openshift.io/cluster-version"

	// Instance ID annotation
	instanceIDAnnotation = "cluster-operator.openshift.io/aws-instance-id"
)

// Actuator is the driver struct for holding AWS machine information
type AWSActuator struct {
	kubeClient    *kubernetes.Clientset
	clusterClient *clusterclient.Clientset
	codecFactory  serializer.CodecFactory
}

// NewAWSDriver returns an empty AWSDriver object
func NewActuator(kubeClient *kubernetes.Clientset, clusterClient *clusterclient.Clientset) *AWSActuator {
	actuator := &AWSActuator{
		kubeClient:    kubeClient,
		clusterClient: clusterClient,
		codecFactory:  coapi.Codecs,
	}
	return actuator
}

// Actuator controls machines on a specific infrastructure. All
// methods should be idempotent unless otherwise specified.
type Actuator interface {
	// Create the machine.
	Create(*clusterv1.Cluster, *clusterv1.Machine) error
	// Delete the machine.
	Delete(*clusterv1.Machine) error
	// Update the machine to the provided definition.
	Update(c *clusterv1.Cluster, machine *clusterv1.Machine) error
	// Checks if the machine currently exists.
	Exists(*clusterv1.Machine) (bool, error)
}

// Create runs a new EC2 instance
func (a *AWSActuator) Create(cluster *clusterv1.Cluster, machine *clusterv1.Machine) error {
	result, err := a.CreateMachine(cluster, machine)
	if err != nil {
		return err
	}
	machineCopy := machine.DeepCopy()
	machineCopy.Annotations[instanceIDAnnotation] = *(result.Instances[0].InstanceId)
	_, err = a.clusterClient.ClusterV1alpha1().Machines(machineCopy.Namespace).Update(machineCopy)
	return err
}

func (a *AWSActuator) CreateMachine(cluster *clusterv1.Cluster, machine *clusterv1.Machine) (*ec2.Reservation, error) {
	svc, err := a.createSVC()
	if err != nil {
		return nil, err
	}

	// Extract cluster operator cluster
	coCluster, err := a.clusterOperatorCluster(cluster)
	if err != nil {
		return nil, err
	}

	if coCluster.Spec.Hardware.AWS == nil {
		return nil, fmt.Errorf("Cluster does not contain an AWS hardware spec")
	}

	region := coCluster.Spec.Hardware.AWS.Region

	// For now, we store the machineSet resource in the machine.
	// It really should be the machine resource, but it's not yet
	// fleshed out in the Cluster Operator API
	coMachineSet, coClusterVersion, err := a.clusterOperatorMachineSet(machine)
	if err != nil {
		return nil, err
	}

	if coClusterVersion.Spec.VMImages.AWSImages == nil {
		return nil, fmt.Errorf("Cluster version does not contain AWS images")
	}

	// Get AMI to use
	amiName := amiForRegion(coClusterVersion.Spec.VMImages.AWSImages.RegionAMIs, region)
	if len(amiName) == 0 {
		return nil, fmt.Errorf("Cannot determine AMI name from cluster version %#v and region %s", coClusterVersion, region)
	}
	imageIds := []*string{aws.String(amiName)}
	describeImagesRequest := ec2.DescribeImagesInput{
		ImageIds: imageIds,
	}
	describeAMIResult, err := svc.DescribeImages(&describeImagesRequest)
	if err != nil {
		return nil, fmt.Errorf("Error describing AMI %s: %v", amiName, err)
	}
	if len(describeAMIResult.Images) != 1 {
		return nil, fmt.Errorf("Unexpected number of images returned: %d", len(describeAMIResult.Images))
	}

	// Describe VPC
	vpcName := coCluster.Name
	vpcNameFilter := "tag:Name"
	describeVpcsRequest := ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{&ec2.Filter{Name: &vpcNameFilter, Values: []*string{&vpcName}}},
	}
	describeVpcsResult, err := svc.DescribeVpcs(&describeVpcsRequest)
	if err != nil {
		return nil, fmt.Errorf("Error describing VPC %s: %v", vpcName, err)
	}
	if len(describeVpcsResult.Vpcs) != 1 {
		return nil, fmt.Errorf("Unexpected number of VPCs: %d", len(describeVpcsResult.Vpcs))
	}
	vpcId := *(describeVpcsResult.Vpcs[0].VpcId)

	// Describe Subnet
	describeSubnetsRequest := ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			&ec2.Filter{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpcId)}},
			&ec2.Filter{Name: aws.String("availabilityZone"), Values: []*string{aws.String(defaultAvailabilityZone)}},
		},
	}
	describeSubnetsResult, err := svc.DescribeSubnets(&describeSubnetsRequest)
	if err != nil {
		return nil, fmt.Errorf("Error describing Subnets for VPC %s: %v", vpcName, err)
	}
	if len(describeSubnetsResult.Subnets) != 1 {
		return nil, fmt.Errorf("Unexpected number of Subnets: %d", len(describeSubnetsResult.Subnets))
	}

	// Determine security groups
	var groupName, groupNameK8s string
	if coMachineSet.Spec.Infra {
		groupName = vpcName + "_infra"
		groupNameK8s = vpcName + "_infra_k8s"
	} else {
		groupName = vpcName + "_compute"
		groupNameK8s = vpcName + "_compute_k8s"
	}
	securityGroupNames := []*string{&vpcName, &groupName, &groupNameK8s}
	sgNameFilter := "group-name"
	describeSecurityGroupsRequest := ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			&ec2.Filter{Name: aws.String("vpc-id"), Values: []*string{&vpcId}},
			&ec2.Filter{Name: &sgNameFilter, Values: securityGroupNames},
		},
	}
	describeSecurityGroupsResult, err := svc.DescribeSecurityGroups(&describeSecurityGroupsRequest)
	if err != nil {
		return nil, err
	}

	var securityGroupIds []*string
	for _, g := range describeSecurityGroupsResult.SecurityGroups {
		groupId := *g.GroupId
		securityGroupIds = append(securityGroupIds, &groupId)
	}

	// build list of networkInterfaces (just 1 for now)
	var networkInterfaces = []*ec2.InstanceNetworkInterfaceSpecification{
		&ec2.InstanceNetworkInterfaceSpecification{
			DeviceIndex:              aws.Int64(0),
			AssociatePublicIpAddress: aws.Bool(true),
			SubnetId:                 describeSubnetsResult.Subnets[0].SubnetId,
			Groups:                   securityGroupIds,
		},
	}

	// Add tags to the created machine
	tagList := []*ec2.Tag{
		&ec2.Tag{Key: aws.String("clusterid"), Value: aws.String(coCluster.Name)},
		&ec2.Tag{Key: aws.String("kubernetes.io/cluster/" + coCluster.Name), Value: aws.String(coCluster.Name)},
		&ec2.Tag{Key: aws.String("Name"), Value: aws.String(machine.Name)},
	}
	tagInstance := &ec2.TagSpecification{
		ResourceType: aws.String("instance"),
		Tags:         tagList,
	}

	// For now, these are fixed
	blkDeviceMappings := []*ec2.BlockDeviceMapping{
		&ec2.BlockDeviceMapping{
			DeviceName: aws.String("/dev/sda1"),
			Ebs: &ec2.EbsBlockDevice{
				DeleteOnTermination: aws.Bool(true),
				VolumeSize:          aws.Int64(100),
				VolumeType:          aws.String("gp2"),
			},
		},
		&ec2.BlockDeviceMapping{
			DeviceName: aws.String("/dev/sdb"),
			Ebs: &ec2.EbsBlockDevice{
				DeleteOnTermination: aws.Bool(true),
				VolumeSize:          aws.Int64(100),
				VolumeType:          aws.String("gp2"),
			},
		},
	}

	bootstrapKubeConfig, err := getBootstrapKubeconfig()
	if err != nil {
		return nil, err
	}
	userData := getUserData(bootstrapKubeConfig, coMachineSet.Spec.Infra)
	userDataEnc := base64.StdEncoding.EncodeToString([]byte(userData))

	inputConfig := ec2.RunInstancesInput{
		ImageId:      describeAMIResult.Images[0].ImageId,
		InstanceType: aws.String(coMachineSet.Spec.Hardware.AWS.InstanceType),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		UserData:     &userDataEnc,
		KeyName:      aws.String(coCluster.Spec.Hardware.AWS.KeyPairName),
		SubnetId:     describeSubnetsResult.Subnets[0].SubnetId,
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{
			Name: aws.String(defaultIAMRole),
		},
		SecurityGroupIds:    securityGroupIds,
		BlockDeviceMappings: blkDeviceMappings,
		TagSpecifications:   []*ec2.TagSpecification{tagInstance},
		NetworkInterfaces:   networkInterfaces,
	}

	runResult, err := svc.RunInstances(&inputConfig)
	if err != nil {
		return nil, err
	}

	return runResult, nil
}

// Delete method is used to delete a AWS machine
func (a *AWSActuator) Delete(machine *clusterv1.Machine) error {
	/*
		var (
			err       error
			machineID = d.decodeMachineID(d.MachineID)
		)

		svc := d.createSVC()
		input := &ec2.TerminateInstancesInput{
			InstanceIds: []*string{
				aws.String(machineID),
			},
			DryRun: aws.Bool(true),
		}
		_, err = svc.TerminateInstances(input)
		awsErr, ok := err.(awserr.Error)
		if ok && awsErr.Code() == "DryRunOperation" {
			input.DryRun = aws.Bool(false)
			output, err := svc.TerminateInstances(input)
			if err != nil {
				glog.Errorf("Could not terminate machine: %s", err.Error())
				return err
			}

			vmState := output.TerminatingInstances[0]
			//glog.Info(vmState.PreviousState, vmState.CurrentState)

			if *vmState.CurrentState.Name == "shutting-down" ||
				*vmState.CurrentState.Name == "terminated" {
				return nil
			}

			err = errors.New("Machine already terminated")
		}

		glog.Errorf("Could not terminate machine: %s", err.Error())
		return err
	*/
	return nil
}

// Update the machine to the provided definition.
// TODO: For now, update is a No-op
func (a *AWSActuator) Update(c *clusterv1.Cluster, machine *clusterv1.Machine) error {
	return nil
}

// Checks if the machine currently exists.
func (a *AWSActuator) Exists(*clusterv1.Machine) (bool, error) {
	return false, nil
}

// Helper function to create SVC
func (a *AWSActuator) createSVC() (*ec2.EC2, error) {
	s, err := session.NewSession()
	if err != nil {
		return nil, err
	}
	return ec2.New(s), nil
}

func (a *AWSActuator) clusterOperatorCluster(c *clusterv1.Cluster) (*cov1.Cluster, error) {
	gvk := cov1.SchemeGroupVersion.WithKind("Cluster")
	obj, _, err := a.codecFactory.UniversalDecoder().Decode([]byte(c.Spec.ProviderConfig), &gvk, nil)
	if err != nil {
		return nil, err
	}
	coCluster, ok := obj.(*cov1.Cluster)
	if !ok {
		return nil, fmt.Errorf("Unexpected object: %#v", obj)
	}
	return coCluster, nil
}

func (a *AWSActuator) clusterOperatorMachineSet(m *clusterv1.Machine) (*cov1.MachineSet, *cov1.ClusterVersion, error) {
	gvk := cov1.SchemeGroupVersion.WithKind("MachineSet")
	obj, _, err := a.codecFactory.UniversalDecoder().Decode(m.Spec.ProviderConfig.Value.Raw, &gvk, nil)
	if err != nil {
		return nil, nil, err
	}
	coMachineSet, ok := obj.(*cov1.MachineSet)
	if !ok {
		return nil, nil, fmt.Errorf("Unexpected machine set object: %#v", obj)
	}
	rawClusterVersion, ok := coMachineSet.Annotations[clusterVersionAnnotation]
	if !ok {
		return nil, nil, fmt.Errorf("Missing ClusterVersion resource annotation in MachineSet %#v", coMachineSet)
	}
	clusterVersionGVK := cov1.SchemeGroupVersion.WithKind("ClusterVersion")
	obj, _, err = a.codecFactory.UniversalDecoder().Decode([]byte(rawClusterVersion), &clusterVersionGVK, nil)
	if err != nil {
		return nil, nil, err
	}
	coClusterVersion, ok := obj.(*cov1.ClusterVersion)
	if !ok {
		return nil, nil, fmt.Errorf("Unexpected cluster version object: %#v", obj)
	}

	return coMachineSet, coClusterVersion, nil
}

func amiForRegion(amis []cov1.AWSRegionAMIs, region string) string {
	for _, a := range amis {
		if a.Region == region {
			return a.AMI
		}
	}
	return ""
}

// template for user data
// takes the following parameters:
// 1 - type of machine (infra/compute)
// 2 - base64-encoded bootstrap.kubeconfig
const userDataTemplate = `#cloud-config
write_files:
- path: /root/openshift_bootstrap/openshift_settings.yaml
  owner: 'root:root'
  permissions: '0640'
  content: |
    openshift_group_type: %[1]s
- path: /etc/origin/node/bootstrap.kubeconfig
  owner: 'root:root'
  permissions: '0640'
  encoding: b64
  content: %[2]s
runcmd:
- [ ansible-playbook, /root/openshift_bootstrap/bootstrap.yml]
- [ systemctl, restart, systemd-hostnamed]
- [ systemctl, restart, NetworkManager]
- [ systemctl, enable, origin-node]
- [ systemctl, start, origin-node]
`

func getUserData(bootstrapKubeConfig string, infra bool) string {
	var nodeType string
	if infra {
		nodeType = "infra"
	} else {
		nodeType = "compute"
	}
	return fmt.Sprintf(userDataTemplate, nodeType, bootstrapKubeConfig)
}

func getBootstrapKubeconfig() (string, error) {
	content, err := ioutil.ReadFile(bootstrapKubeConfig)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(content), nil
}
