package k8s

import (
	"fmt"
	"github.com/foxdalas/deploy-checker/pkg/checker_const"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func New(checker checker.Checker, kubeconfig string, namespace string) (*k8s, error) {
	var config *rest.Config
	var err error

	if (os.Getenv("KUBECONFIG_CONTENT") != "") {
		checker.Log().Info("Using configuration from environment value KUBECONFIG_CONTENT")
		config, err = clientcmd.RESTConfigFromKubeConfig([]byte(os.Getenv("KUBECONFIG_CONTENT")))
		if err != nil {
			return nil, err
		}
	} else {
		config, err = rest.InClusterConfig()
		if err != nil {
			checker.Log().Warnf("failed to create in-cluster client: %v.", err)
			config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
			if err != nil {
				return nil, err
			}
		}
	}

	clientset, err := kubernetes.NewForConfig(config)

	return &k8s{
		checker:   checker,
		client:    clientset,
		namespace: namespace,
	}, err
}

func (k *k8s) isDeploymentExist(name string) bool {
	deploymentsClient := k.client.AppsV1().Deployments(k.namespace)
	_, err := deploymentsClient.Get(name, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return true
}

func (k *k8s) getKubernetesDeployment(name string) *v1beta1.Deployment {
	deploymentsClient := k.client.Extensions().Deployments(k.namespace)
	obj, err := deploymentsClient.Get(name, metav1.GetOptions{})
	if err != nil {
		k.Log().Fatal(err)
	}

	return obj
}

func (k *k8s) getDeploymentFile(path string) {
	dat, err := ioutil.ReadFile(path)
	if err != nil {
		k.Log().Error(err)
	}

	decode := scheme.Codecs.UniversalDeserializer().Decode
	obj, _, err := decode([]byte(dat), nil, nil)
	if err != nil {
		k.Log().Fatal(fmt.Sprintf("Error while decoding YAML object. Err was: %s", err))
	}
	switch o := obj.(type) {
	case *v1beta1.Deployment:
		k.yamlDeployment = o
	default:
		k.Log().Fatalf("File %s is not a kubernetes deployment", k.deploymentFile)
	}
}

func (k *k8s) updateDeploymentFile(path string) {
	if *k.k8sDeployment.Spec.Replicas != *k.yamlDeployment.Spec.Replicas {
		k.Log().Infof("Current deployment is changed. Replicas in repository %d and %d replicas in k8s", *k.yamlDeployment.Spec.Replicas,
			*k.k8sDeployment.Spec.Replicas)
	} else {
		return
	}

	//Fix replicas
	*k.yamlDeployment.Spec.Replicas = *k.k8sDeployment.Spec.Replicas

	f, err := os.Create(path)
	if err != nil {
		k.Log().Fatal(err)
	}
	defer f.Close()

	k.Log().Infof("Updating file %s", path)
	s := json.NewYAMLSerializer(json.DefaultMetaFactory, nil, nil)
	err = s.Encode(k.yamlDeployment, f)
	if err != nil {
		k.Log().Fatal(err)
	}
}

func (k *k8s) PrepareDeployment() {
	for _, path := range k.findDeployments(".") {
		k.getDeploymentFile(path)

		if k.isDeploymentExist(k.yamlDeployment.Name) {
			k.k8sDeployment = k.getKubernetesDeployment(k.yamlDeployment.Name)
			k.updateDeploymentFile(path)
		} else {
			k.Log().Infof("Deployment not found in kubernetes. Is a new deploy %s", k.yamlDeployment.Name)
		}
	}
}

func (k *k8s) DeploymentProgress(deployment *v1beta1.Deployment) v1beta1.DeploymentConditionType {
	conditions := deployment.Status.Conditions
	lastCondition := conditions[len(k.k8sDeployment.Status.Conditions)-1]
	return lastCondition.Type
}

func (k *k8s) deploymentInProgress(name string) (string, bool, error) {
	deployment, err := k.client.Extensions().Deployments(k.namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		k.Log().Error(err)
	}
	if deployment.Generation <= deployment.Status.ObservedGeneration {
		cond := getDeploymentCondition(deployment.Status, v1beta1.DeploymentProgressing)
		if cond != nil && cond.Reason == TimedOutReason {
			return "", false, fmt.Errorf("deployment %q exceeded its progress deadline", name)
		}
		if deployment.Spec.Replicas != nil && deployment.Status.UpdatedReplicas < *deployment.Spec.Replicas {
			return fmt.Sprintf("Waiting for deployment %q rollout to finish: %d out of %d new replicas have been updated...", name, deployment.Status.UpdatedReplicas, *deployment.Spec.Replicas), false, nil
		}
		if deployment.Status.Replicas > deployment.Status.UpdatedReplicas {
			return fmt.Sprintf("Waiting for deployment %q rollout to finish: %d old replicas are pending termination...", name, deployment.Status.Replicas-deployment.Status.UpdatedReplicas), false, nil
		}
		if deployment.Status.AvailableReplicas < deployment.Status.UpdatedReplicas {
			return fmt.Sprintf("Waiting for deployment %q rollout to finish: %d of %d updated replicas are available...", name, deployment.Status.AvailableReplicas, deployment.Status.UpdatedReplicas), false, nil
		}
		return fmt.Sprintf("deployment %q successfully rolled out", name), true, nil
	}
	return fmt.Sprintf("Waiting for deployment spec update to be observed..."), false, nil
}

func getDeploymentCondition(status v1beta1.DeploymentStatus, condType v1beta1.DeploymentConditionType) *v1beta1.DeploymentCondition {
	for i := range status.Conditions {
		c := status.Conditions[i]
		if c.Type == condType {
			return &c
		}
	}
	return nil
}

func (k *k8s) Wait(name string, wg *sync.WaitGroup) error {
	defer wg.Done()
	var message string
	ticker := 0
	for {
		state, status, err := k.deploymentInProgress(name)
		if err != nil {
			k.Log().Error(err)
			return err
		}
		if message != state {
			k.Log().Info(state)
			message = state
		}
		if status {
			return nil
		}
		time.Sleep(time.Second * 5)
		ticker++
	}
}

func (k *k8s) Log() *log.Entry {
	return k.checker.Log().WithField("context", "k8s")
}

func (k *k8s) findDeployments(searchDir string) []string {
	fileList := []string{}
	err := filepath.Walk(searchDir, func(path string, f os.FileInfo, err error) error {
		if strings.Contains(path, "deployment.yml") {
			fileList = append(fileList, path)
			k.Log().Infof("Founded deployment file %s", path)
		}
		return nil
	})
	if err != nil {
		k.Log().Error(err)
	}

	return fileList
}
