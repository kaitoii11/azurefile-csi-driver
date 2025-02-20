/*
Copyright 2017 The Kubernetes Authors.

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

package azurefile

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2020-08-01/network"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	azureprovider "sigs.k8s.io/cloud-provider-azure/pkg/provider"
)

const (
	DefaultAzureCredentialFileEnv = "AZURE_CREDENTIAL_FILE"
	DefaultCredFilePathLinux      = "/etc/kubernetes/azure.json"
	DefaultCredFilePathWindows    = "C:\\k\\azure.json"
)

var (
	storageService = "Microsoft.Storage"
)

// getCloudProvider get Azure Cloud Provider
func getCloudProvider(kubeconfig, nodeID string) (*azureprovider.Cloud, error) {
	az := &azureprovider.Cloud{}
	kubeClient, err := getKubeClient(kubeconfig)
	if err != nil && !os.IsNotExist(err) && err != rest.ErrNotInCluster {
		return az, fmt.Errorf("failed to get KubeClient: %v", err)
	}

	if kubeClient != nil {
		klog.V(2).Infof("reading cloud config from secret")
		az.KubeClient = kubeClient
		az.InitializeCloudFromSecret()
	}

	if az.TenantID == "" || az.SubscriptionID == "" || az.ResourceGroup == "" {
		klog.V(2).Infof("could not read cloud config from secret")
		credFile, ok := os.LookupEnv(DefaultAzureCredentialFileEnv)
		if ok {
			klog.V(2).Infof("%s env var set as %v", DefaultAzureCredentialFileEnv, credFile)
		} else {
			if runtime.GOOS == "windows" {
				credFile = DefaultCredFilePathWindows
			} else {
				credFile = DefaultCredFilePathLinux
			}
			klog.V(2).Infof("use default %s env var: %v", DefaultAzureCredentialFileEnv, credFile)
		}

		var f *os.File
		f, err = os.Open(credFile)
		if f != nil {
			defer f.Close()
		}
		if err != nil {
			klog.Errorf("Failed to load config from file: %s", credFile)
			err = fmt.Errorf("Failed to load config from file: %s, cloud not get azure cloud provider", credFile)
		} else {
			klog.V(2).Infof("read cloud config from file: %s successfully", credFile)
			az, err = azureprovider.NewCloudWithoutFeatureGates(f)
		}
	}

	// reassign kubeClient
	if kubeClient != nil && az.KubeClient == nil {
		az.KubeClient = kubeClient
	}

	if err != nil {
		klog.V(2).Infof("no cloud config provided, error: %v, driver will run without cloud config", err)
	}

	isController := (nodeID == "")
	if isController {
		if err == nil {
			// Disable UseInstanceMetadata for controller to mitigate a timeout issue using IMDS
			// https://github.com/kubernetes-sigs/azuredisk-csi-driver/issues/168
			klog.V(2).Infof("disable UseInstanceMetadata for controller server")
			az.Config.UseInstanceMetadata = false
		}
		klog.V(2).Infof("starting controller server...")
	} else {
		klog.V(2).Infof("starting node server on node(%s)", nodeID)
	}

	return az, nil
}

func getKubeClient(kubeconfig string) (*kubernetes.Clientset, error) {
	var (
		config *rest.Config
		err    error
	)
	if kubeconfig != "" {
		if config, err = clientcmd.BuildConfigFromFlags("", kubeconfig); err != nil {
			return nil, err
		}
	} else {
		if config, err = rest.InClusterConfig(); err != nil {
			return nil, err
		}
	}

	return kubernetes.NewForConfig(config)
}

func (d *Driver) updateSubnetServiceEndpoints(ctx context.Context) error {
	if d.cloud.SubnetsClient == nil {
		return fmt.Errorf("SubnetsClient is nil")
	}

	resourceGroup := d.cloud.ResourceGroup
	if len(d.cloud.VnetResourceGroup) > 0 {
		resourceGroup = d.cloud.VnetResourceGroup
	}
	location := d.cloud.Location
	vnetName := d.cloud.VnetName
	subnetName := d.cloud.SubnetName

	klog.V(2).Infof("updateSubnetServiceEndpoints on VnetName: %s, SubnetName: %s", vnetName, subnetName)

	lockKey := resourceGroup + vnetName + subnetName
	d.subnetLockMap.LockEntry(lockKey)
	defer d.subnetLockMap.UnlockEntry(lockKey)

	subnet, err := d.cloud.SubnetsClient.Get(ctx, resourceGroup, vnetName, subnetName, "")
	if err != nil {
		return fmt.Errorf("failed to get the subnet %s under vnet %s: %v", subnetName, vnetName, err)
	}
	endpointLocaions := []string{location}
	storageServiceEndpoint := network.ServiceEndpointPropertiesFormat{
		Service:   &storageService,
		Locations: &endpointLocaions,
	}
	storageServiceExists := false
	if subnet.SubnetPropertiesFormat == nil {
		subnet.SubnetPropertiesFormat = &network.SubnetPropertiesFormat{}
	}
	if subnet.SubnetPropertiesFormat.ServiceEndpoints == nil {
		subnet.SubnetPropertiesFormat.ServiceEndpoints = &[]network.ServiceEndpointPropertiesFormat{}
	}
	serviceEndpoints := *subnet.SubnetPropertiesFormat.ServiceEndpoints
	for _, v := range serviceEndpoints {
		if v.Service != nil && *v.Service == storageService {
			storageServiceExists = true
			klog.V(4).Infof("serviceEndpoint(%s) is already in subnet(%s)", storageService, subnetName)
			break
		}
	}

	if !storageServiceExists {
		serviceEndpoints = append(serviceEndpoints, storageServiceEndpoint)
		subnet.SubnetPropertiesFormat.ServiceEndpoints = &serviceEndpoints

		err = d.cloud.SubnetsClient.CreateOrUpdate(context.Background(), resourceGroup, vnetName, subnetName, subnet)
		if err != nil {
			return fmt.Errorf("failed to update the subnet %s under vnet %s: %v", subnetName, vnetName, err)
		}
		klog.V(2).Infof("serviceEndpoint(%s) is appended in subnet(%s)", storageService, subnetName)
	}

	return nil
}
