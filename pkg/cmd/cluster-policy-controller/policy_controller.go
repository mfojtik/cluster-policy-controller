package cluster_policy_controller

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/openshift/library-go/pkg/serviceability"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authorizationv1client "k8s.io/client-go/kubernetes/typed/authorization/v1"

	"k8s.io/klog"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/kubernetes/pkg/api/legacyscheme"

	openshiftcontrolplanev1 "github.com/openshift/api/openshiftcontrolplane/v1"
	origincontrollers "github.com/openshift/cluster-policy-controller/pkg/cmd/controller"
	"github.com/openshift/cluster-policy-controller/pkg/version"

	// for metrics
	_ "k8s.io/component-base/metrics/prometheus/restclient"
)

func RunClusterPolicyController(config *openshiftcontrolplanev1.OpenShiftControllerManagerConfig, clientConfig *rest.Config) error {
	serviceability.InitLogrusFromKlog()
	kubeClient, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return err
	}

	// only serve if we have serving information.
	if config.ServingInfo != nil {
		klog.Infof("Starting controllers on %s (%s)", config.ServingInfo.BindAddress, version.Get().String())

		if err := origincontrollers.RunControllerServer(*config.ServingInfo, kubeClient); err != nil {
			return err
		}
	}

	originControllerManager := func(ctx context.Context) {
		if err := WaitForHealthyAPIServer(kubeClient.Discovery().RESTClient()); err != nil {
			klog.Fatal(err)
		}
		if err := WaitForAuthorizationUpdate(kubeClient.AuthorizationV1()); err != nil {
			klog.Fatal(err)
		}

		controllerContext, err := origincontrollers.NewControllerContext(*config, clientConfig, ctx.Done())
		if err != nil {
			klog.Fatal(err)
		}
		if err := startControllers(controllerContext); err != nil {
			klog.Fatal(err)
		}
		controllerContext.StartInformers(ctx.Done())
	}

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	eventRecorder := eventBroadcaster.NewRecorder(legacyscheme.Scheme, v1.EventSource{Component: "cluster-policy-controller"})
	id, err := os.Hostname()
	if err != nil {
		return err
	}
	rl, err := resourcelock.New(
		"configmaps",
		// namespace where cluster-policy-controller container runs in static pod
		"openshift-kube-controller-manager",
		"cluster-policy-controller",
		kubeClient.CoreV1(),
		kubeClient.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity:      id,
			EventRecorder: eventRecorder,
		})
	if err != nil {
		return err
	}
	go leaderelection.RunOrDie(context.Background(),
		leaderelection.LeaderElectionConfig{
			Lock:          rl,
			LeaseDuration: config.LeaderElection.LeaseDuration.Duration,
			RenewDeadline: config.LeaderElection.RenewDeadline.Duration,
			RetryPeriod:   config.LeaderElection.RetryPeriod.Duration,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: originControllerManager,
				OnStoppedLeading: func() {
					klog.Fatalf("leaderelection lost")
				},
			},
		})

	return nil
}

func WaitForHealthyAPIServer(client rest.Interface) error {
	var healthzContent string
	// If apiserver is not running we should wait for some time and fail only then. This is particularly
	// important when we start apiserver and controller manager at the same time.
	err := wait.PollImmediate(time.Second, 5*time.Minute, func() (bool, error) {
		healthStatus := 0
		resp := client.Get().AbsPath("/healthz").Do(context.TODO()).StatusCode(&healthStatus)
		if healthStatus != http.StatusOK {
			klog.Errorf("Server isn't healthy yet. Waiting a little while.")
			return false, nil
		}
		content, _ := resp.Raw()
		healthzContent = string(content)

		return true, nil
	})
	if err != nil {
		return fmt.Errorf("server unhealthy: %v: %v", healthzContent, err)
	}

	return nil
}

func WaitForAuthorizationUpdate(client authorizationv1client.SubjectAccessReviewsGetter) error {
	review := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Group:     "",
				Verb:      "get",
				Resource:  "configmaps",
				Namespace: "openshift-kube-controller-manager",
			},
			User: "system:kube-controller-manager",
		},
	}
	if err := wait.PollImmediate(time.Second, 2*time.Minute, func() (bool, error) {
		response, err := client.SubjectAccessReviews().Create(context.TODO(), review, metav1.CreateOptions{})
		if err != nil {
			return false, err
		}
		if !response.Status.Allowed {
			klog.Infof("Waiting for system:kube-controller-manager to be able to access configmaps... ")
			return false, nil
		}
		return true, nil
	}); err != nil {
		return fmt.Errorf("server missing RBAC policy for system:kube-controller-manager: %v", err)
	}

	return nil
}

// startControllers launches the controllers
// allocation controller is passed in because it wants direct etcd access.  Naughty.
func startControllers(controllerContext *origincontrollers.ControllerContext) error {
	for controllerName, initFn := range origincontrollers.ControllerInitializers {
		if !controllerContext.IsControllerEnabled(controllerName) {
			klog.Warningf("%q is disabled", controllerName)
			continue
		}

		klog.V(1).Infof("Starting %q", controllerName)
		started, err := initFn(controllerContext)
		if err != nil {
			klog.Fatalf("Error starting %q (%v)", controllerName, err)
			return err
		}
		if !started {
			klog.Warningf("Skipping %q", controllerName)
			continue
		}
		klog.Infof("Started %q", controllerName)
	}

	klog.Infof("Started Origin Controllers")

	return nil
}
