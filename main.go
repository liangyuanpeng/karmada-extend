package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"

	policyv1alpha1 "github.com/karmada-io/karmada/pkg/apis/policy/v1alpha1"
	workv1alpha2 "github.com/karmada-io/karmada/pkg/apis/work/v1alpha2"
	karmadaclientset "github.com/karmada-io/karmada/pkg/generated/clientset/versioned"
	informerfactory "github.com/karmada-io/karmada/pkg/generated/informers/externalversions"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubeclientset "k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	setupLog      = ctrl.Log.WithName("setup")
	karmadaClient *karmadaclientset.Clientset
	kubeClient    *kubeclientset.Clientset
	PREFIX_NAME   = "kextend-"
)

func onAdd(obj interface{}, isInInitialList bool) {
	obj2 := obj.(*workv1alpha2.ResourceBinding)
	err := reconcileRB(obj2)
	if err != nil {
		klog.Error("reconcile onAdd failed!", err)
	}
}

func onUpdate(old, new interface{}) {
	obj := new.(*workv1alpha2.ResourceBinding)
	err := reconcileRB(obj)
	if err != nil {
		klog.Error("reconcile onUpdate failed!", err)
	}
}

func reconcileRB(wobj interface{}) error {
	obj := wobj.(*workv1alpha2.ResourceBinding)
	if strings.HasPrefix(obj.Name, PREFIX_NAME) {
		return nil
	}
	if len(obj.Spec.Clusters) <= 0 {
		return nil
	}
	kextentNS := "karmada-system"
	kextentNS = obj.Namespace
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "work.karmada.io",
		Version: "v1alpha2",
		Kind:    "ResourceBinding",
	})
	data := make(map[string]string)

	matchClusters := []string{}
	for _, c := range obj.Spec.Clusters {
		data[c.Name] = strconv.FormatInt(int64(c.Replicas), 10)
		matchClusters = append(matchClusters, c.Name)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PREFIX_NAME + obj.Name,
			Namespace: kextentNS,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(obj, obj.GroupVersionKind()),
			},
		},
		Data: data,
	}
	log.Println("reconcile to createOrUpdate configmap:", cm.Name)
	if _, err := kubeClient.CoreV1().ConfigMaps(kextentNS).Create(context.TODO(), cm, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("unable to create ConfigMap: %v", err)
		}

		existCm, err := kubeClient.CoreV1().ConfigMaps(cm.Namespace).Get(context.TODO(), cm.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("error:%v", err)
		}

		cm.ResourceVersion = existCm.ResourceVersion

		if _, err := kubeClient.CoreV1().ConfigMaps(cm.Namespace).Update(context.TODO(), cm, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("unable to update ConfigMap: %v", err)
		}
	}

	clusterDatas := make(map[string]map[string]string)

	cm, err := kubeClient.CoreV1().ConfigMaps(cm.Namespace).Get(context.TODO(), cm.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error:%v", err)
	}

	op := &policyv1alpha1.OverridePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PREFIX_NAME + obj.Name,
			Namespace: kextentNS,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(obj, obj.GroupVersionKind()),
			},
		},
		Spec: policyv1alpha1.OverrideSpec{
			ResourceSelectors: []policyv1alpha1.ResourceSelector{
				{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       cm.Name,
					Namespace:  cm.Namespace,
				},
			},
			OverrideRules: []policyv1alpha1.RuleWithCluster{},
		},
	}

	err = createPPOrUpdate(kextentNS, obj, matchClusters)
	if err != nil {
		return nil
	}

	for _, c := range obj.Spec.Clusters {
		clusterdata := make(map[string]string)
		for name, v := range data {
			if name == c.Name {
				clusterdata["self"] = v
			} else {
				clusterdata[name] = v
			}
		}
		clusterDatas[c.Name] = clusterdata

		addOverrideRules(op, clusterDatas, c.Name)

	}

	return createOPOrUpdate(op)
}

func createPPOrUpdate(namespace string, obj *workv1alpha2.ResourceBinding, matchClusters []string) error {
	pp := &policyv1alpha1.PropagationPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PREFIX_NAME + obj.Name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(obj, obj.GroupVersionKind()),
			},
		},
		Spec: policyv1alpha1.PropagationSpec{
			ResourceSelectors: []policyv1alpha1.ResourceSelector{
				{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Name:       PREFIX_NAME + obj.Name,
					Namespace:  namespace,
				},
			},
			Placement: policyv1alpha1.Placement{
				ClusterAffinity: &policyv1alpha1.ClusterAffinity{
					ClusterNames: matchClusters,
				},
			},
		},
	}

	if _, err := karmadaClient.PolicyV1alpha1().PropagationPolicies(pp.Namespace).Create(context.TODO(), pp, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("unable to create PropagationPolicies: %v", err)
		}

		existCm, err := karmadaClient.PolicyV1alpha1().PropagationPolicies(pp.Namespace).Get(context.TODO(), pp.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("error:%v", err)
		}

		pp.ResourceVersion = existCm.ResourceVersion

		if _, err := karmadaClient.PolicyV1alpha1().PropagationPolicies(pp.Namespace).Update(context.TODO(), pp, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("unable to update PropagationPolicies: %v", err)
		}
	}
	return nil
}

func createOPOrUpdate(op *policyv1alpha1.OverridePolicy) error {
	if _, err := karmadaClient.PolicyV1alpha1().OverridePolicies(op.Namespace).Create(context.TODO(), op, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("unable to create OverridePolicies: %v", err)
		}

		existCm, err := karmadaClient.PolicyV1alpha1().OverridePolicies(op.Namespace).Get(context.TODO(), op.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("error:%v", err)
		}

		op.ResourceVersion = existCm.ResourceVersion

		if _, err := karmadaClient.PolicyV1alpha1().OverridePolicies(op.Namespace).Update(context.TODO(), op, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("unable to update OverridePolicies: %v", err)
		}
	}
	return nil
}

func addOverrideRules(op *policyv1alpha1.OverridePolicy, clusterDatas map[string]map[string]string, cluster string) {

	jsonValue, _ := json.Marshal(clusterDatas[cluster])

	rwc := policyv1alpha1.RuleWithCluster{
		TargetCluster: &policyv1alpha1.ClusterAffinity{
			ClusterNames: []string{cluster},
		},
		Overriders: policyv1alpha1.Overriders{
			Plaintext: []policyv1alpha1.PlaintextOverrider{
				{
					Operator: "replace",
					Path:     "/data",
					Value:    apiextensionsv1.JSON{Raw: jsonValue},
				},
			},
		},
	}
	op.Spec.OverrideRules = append(op.Spec.OverrideRules, rwc)
}

func main() {
	ctrl.SetLogger(zap.New())

	kubeconfigPath := "/etc/kubeconfig"
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		panic(err.Error())
	}

	kubeClient = kubeclientset.NewForConfigOrDie(cfg)
	karmadaClient = karmadaclientset.NewForConfigOrDie(cfg)

	factory := informerfactory.NewSharedInformerFactory(karmadaClient, 0)
	bindingInformer := factory.Work().V1alpha2().ResourceBindings().Informer()
	bindingInformer.AddEventHandler(cache.ResourceEventHandlerDetailedFuncs{
		AddFunc:    onAdd,
		UpdateFunc: onUpdate,
	})

	gvr := schema.GroupVersionResource{
		Group:    "work.karmada.io",
		Version:  "v1alpha2",
		Resource: "ResourceBinding",
	}
	_, _ = factory.ForResource(gvr)

	ctx := context.TODO()

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	klog.Info("karmada-extend started")
	<-ctx.Done()
}
