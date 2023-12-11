package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	policyv1alpha1 "github.com/karmada-io/karmada/pkg/apis/policy/v1alpha1"
	workv1alpha2 "github.com/karmada-io/karmada/pkg/apis/work/v1alpha2"
	karmadaclientset "github.com/karmada-io/karmada/pkg/generated/clientset/versioned"
	informerfactory "github.com/karmada-io/karmada/pkg/generated/informers/externalversions"
	worklister "github.com/karmada-io/karmada/pkg/generated/listers/work/v1alpha2"
	"github.com/karmada-io/karmada/pkg/scheduler/metrics"
	"github.com/karmada-io/karmada/pkg/sharedcli/ratelimiterflag"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

var (
	PREFIX_NAME = "kextend-"
)

// KarmadaExtend
type KarmadaExtend struct {
	DynamicClient   dynamic.Interface
	KarmadaClient   karmadaclientset.Interface
	KubeClient      kubernetes.Interface
	bindingLister   worklister.ResourceBindingLister
	informerFactory informerfactory.SharedInformerFactory
	queue           workqueue.RateLimitingInterface

	eventRecorder record.EventRecorder

	enableEmptyWorkloadPropagation bool
}

type schedulerOptions struct {
	//enableEmptyWorkloadPropagation represents whether allow workload with replicas 0 propagated to member clusters should be enabled
	enableEmptyWorkloadPropagation bool
	// contains the options for rate limiter.
	RateLimiterOptions ratelimiterflag.Options
}

// Option configures a Scheduler
type Option func(*schedulerOptions)

// WithEnableEmptyWorkloadPropagation sets the enablePropagateEmptyWorkLoad for scheduler
func WithEnableEmptyWorkloadPropagation(enableEmptyWorkloadPropagation bool) Option {
	return func(o *schedulerOptions) {
		o.enableEmptyWorkloadPropagation = enableEmptyWorkloadPropagation
	}
}

// WithRateLimiterOptions sets the rateLimiterOptions for scheduler
func WithRateLimiterOptions(rateLimiterOptions ratelimiterflag.Options) Option {
	return func(o *schedulerOptions) {
		o.RateLimiterOptions = rateLimiterOptions
	}
}

// NewScheduler instantiates a scheduler
func NewScheduler(dynamicClient dynamic.Interface, karmadaClient karmadaclientset.Interface, kubeClient kubernetes.Interface, opts ...Option) (*KarmadaExtend, error) {
	factory := informerfactory.NewSharedInformerFactory(karmadaClient, 0)
	bindingLister := factory.Work().V1alpha2().ResourceBindings().Lister()

	options := schedulerOptions{}
	for _, opt := range opts {
		opt(&options)
	}
	queue := workqueue.NewRateLimitingQueueWithConfig(ratelimiterflag.DefaultControllerRateLimiter(options.RateLimiterOptions), workqueue.RateLimitingQueueConfig{Name: "karmada-extend-queue"})

	sched := &KarmadaExtend{
		DynamicClient:   dynamicClient,
		KarmadaClient:   karmadaClient,
		KubeClient:      kubeClient,
		bindingLister:   bindingLister,
		informerFactory: factory,
		queue:           queue,
	}
	sched.enableEmptyWorkloadPropagation = options.enableEmptyWorkloadPropagation

	sched.addAllEventHandlers()
	return sched, nil
}

// Run runs the extend
func (s *KarmadaExtend) Run(ctx context.Context) {
	stopCh := ctx.Done()
	klog.Infof("Starting karmada extend")
	defer klog.Infof("Shutting down karmada-extend")

	s.informerFactory.Start(stopCh)
	s.informerFactory.WaitForCacheSync(stopCh)

	// s.clusterReconcileWorker.Run(1, stopCh)

	go wait.Until(s.worker, time.Second, stopCh)

	<-stopCh
}

func (s *KarmadaExtend) worker() {
	for s.reconcileNext() {
	}
}

func (s *KarmadaExtend) reconcileNext() bool {
	key, shutdown := s.queue.Get()
	if shutdown {
		klog.Errorf("Fail to pop item from queue")
		return false
	}
	defer s.queue.Done(key)

	err := s.doReconcile(key.(string))
	s.handleErr(err, key)
	return true
}

func (s *KarmadaExtend) doReconcile(key string) error {
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.Info("doReconcile:", " name: ", name, " ,namespace: ", ns)

	rb, err := s.bindingLister.ResourceBindings(ns).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// the binding does not exist, do nothing
			return nil
		}
		return err
	}
	rb = rb.DeepCopy()
	return s.generateConfigMapFromRB(rb)
}

func (s *KarmadaExtend) generateConfigMapFromRB(rb *workv1alpha2.ResourceBinding) error {
	if strings.HasPrefix(rb.Name, PREFIX_NAME) {
		return nil
	}
	if len(rb.Spec.Clusters) <= 0 {
		return nil
	}
	kextendNS := rb.Namespace
	data := make(map[string]string)

	matchClusters := []string{}
	for _, c := range rb.Spec.Clusters {
		data[c.Name] = strconv.FormatInt(int64(c.Replicas), 10)
		matchClusters = append(matchClusters, c.Name)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PREFIX_NAME + rb.Name,
			Namespace: kextendNS,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(rb, rb.GroupVersionKind()),
			},
		},
		Data: data,
	}
	log.Println("reconcile to createOrUpdate configmap:", cm.Name)
	if _, err := s.KubeClient.CoreV1().ConfigMaps(kextendNS).Create(context.TODO(), cm, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("unable to create ConfigMap: %v", err)
		}

		existCm, err := s.KubeClient.CoreV1().ConfigMaps(cm.Namespace).Get(context.TODO(), cm.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("error:%v", err)
		}

		cm.ResourceVersion = existCm.ResourceVersion

		if _, err := s.KubeClient.CoreV1().ConfigMaps(cm.Namespace).Update(context.TODO(), cm, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("unable to update ConfigMap: %v", err)
		}
	}

	clusterDatas := make(map[string]map[string]string)

	cm, err := s.KubeClient.CoreV1().ConfigMaps(cm.Namespace).Get(context.TODO(), cm.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error:%v", err)
	}

	op := &policyv1alpha1.OverridePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PREFIX_NAME + rb.Name,
			Namespace: kextendNS,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(rb, rb.GroupVersionKind()),
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

	err = s.createPPOrUpdate(kextendNS, rb, matchClusters)
	if err != nil {
		return nil
	}

	for _, c := range rb.Spec.Clusters {
		clusterdata := make(map[string]string)
		for name, v := range data {
			if name == c.Name {
				clusterdata["self"] = v
			} else {
				clusterdata[name] = v
			}
		}
		clusterDatas[c.Name] = clusterdata

		s.addOverrideRules(op, clusterDatas, c.Name)
	}

	return s.createOPOrUpdate(op)
}

func (s *KarmadaExtend) createPPOrUpdate(namespace string, obj *workv1alpha2.ResourceBinding, matchClusters []string) error {
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

	if _, err := s.KarmadaClient.PolicyV1alpha1().PropagationPolicies(pp.Namespace).Create(context.TODO(), pp, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("unable to create PropagationPolicies: %v", err)
		}

		existCm, err := s.KarmadaClient.PolicyV1alpha1().PropagationPolicies(pp.Namespace).Get(context.TODO(), pp.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("error:%v", err)
		}

		pp.ResourceVersion = existCm.ResourceVersion

		if _, err := s.KarmadaClient.PolicyV1alpha1().PropagationPolicies(pp.Namespace).Update(context.TODO(), pp, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("unable to update PropagationPolicies: %v", err)
		}
	}
	return nil
}

func (s *KarmadaExtend) createOPOrUpdate(op *policyv1alpha1.OverridePolicy) error {
	if _, err := s.KarmadaClient.PolicyV1alpha1().OverridePolicies(op.Namespace).Create(context.TODO(), op, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("unable to create OverridePolicies: %v", err)
		}

		existCm, err := s.KarmadaClient.PolicyV1alpha1().OverridePolicies(op.Namespace).Get(context.TODO(), op.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("error:%v", err)
		}

		op.ResourceVersion = existCm.ResourceVersion

		if _, err := s.KarmadaClient.PolicyV1alpha1().OverridePolicies(op.Namespace).Update(context.TODO(), op, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("unable to update OverridePolicies: %v", err)
		}
	}
	return nil
}

func (s *KarmadaExtend) addOverrideRules(op *policyv1alpha1.OverridePolicy, clusterDatas map[string]map[string]string, cluster string) {
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

func (s *KarmadaExtend) handleErr(err error, key interface{}) {
	if err == nil || apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
		s.queue.Forget(key)
		return
	}

	s.queue.AddRateLimited(key)
	metrics.CountSchedulerBindings(metrics.ScheduleAttemptFailure)
}
