package k8shandler

import (
	"fmt"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/ViaQ/elasticsearch-operator/pkg/apis/elasticsearch/v1alpha1"
	"github.com/operator-framework/operator-sdk/pkg/sdk"
	//"github.com/sirupsen/logrus"
)

// CreateOrUpdateServices ensures the existence of the services for Elasticsearch cluster
func CreateOrUpdateServices(dpl *v1alpha1.Elasticsearch) error {
	elasticsearchClusterSvcName := fmt.Sprintf("%s-%s", dpl.Name, "cluster")
	elasticsearchRestSvcName := dpl.Name
	owner := asOwner(dpl)

	err := createOrUpdateService(elasticsearchClusterSvcName, dpl.Namespace, dpl.Name, 9300, selectorForES("es-node-master", dpl.Name), owner)
	if err != nil {
		return fmt.Errorf("Failure creating service %v", err)
	}

	err = createOrUpdateService(elasticsearchRestSvcName, dpl.Namespace, dpl.Name, 9200, selectorForES("es-node-client", dpl.Name), owner)
	if err != nil {
		return fmt.Errorf("Failure creating service %v", err)
	}
	return nil
}

func createOrUpdateService(serviceName string, namespace string, clusterName string, port int32, selector map[string]string, owner metav1.OwnerReference) error {
	elasticsearchSvc := createService(serviceName, namespace, clusterName, port, selector)
	addOwnerRefToObject(elasticsearchSvc, owner)
	err := sdk.Create(elasticsearchSvc)
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("Failure constructing Elasticsearch service: %v", err)
	} else if errors.IsAlreadyExists(err) {
		// Get existing service to check if it is same as what we want
		existingSvc := service(serviceName, namespace)
		err = sdk.Get(existingSvc)
		if err != nil {
			return fmt.Errorf("Unable to get Elasticsearch cluster service: %v", err)
		}

		// TODO: Compare existing service labels, selectors and port
	}
	return nil
}

func createService(serviceName string, namespace string, clusterName string, port int32, selector map[string]string) *v1.Service {
	svc := service(serviceName, namespace)
	svc.Spec = v1.ServiceSpec{
		Selector: selector,
		Ports: []v1.ServicePort{
			v1.ServicePort{
				Port:     port,
				Protocol: "TCP",
			},
		},
	}
	return svc
}

// service returns a v1.Service object
func service(serviceName string, namespace string) *v1.Service {
	return &v1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: namespace,
		},
	}
}
