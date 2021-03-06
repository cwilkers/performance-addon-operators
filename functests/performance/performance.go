package performance

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	testutils "github.com/openshift-kni/performance-addon-operators/functests/utils"
	testclient "github.com/openshift-kni/performance-addon-operators/functests/utils/client"
	"github.com/openshift-kni/performance-addon-operators/functests/utils/nodes"
	"github.com/openshift-kni/performance-addon-operators/pkg/controller/performanceprofile/components"
	ocv1 "github.com/openshift/api/config/v1"
	tunedv1 "github.com/openshift/cluster-node-tuning-operator/pkg/apis/tuned/v1"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

var profileName string

func init() {
	profileName = os.Getenv("PERF_TEST_PROFILE")
	if profileName == "" {
		profileName = "ci"
	}
}

const (
	testTimeout      = 480
	testPollInterval = 2
)

var _ = Describe("performance", func() {

	var workerRTNodes []corev1.Node

	BeforeEach(func() {
		var err error
		workerRTNodes, err = nodes.GetByRole(testclient.Client, testutils.RoleWorkerRT)
		Expect(err).ToNot(HaveOccurred())
		Expect(workerRTNodes).ToNot(BeEmpty())
	})

	Context("Pre boot tuning adjusted by the Machine Config Operator ", func() {

		It("Should set workqueue CPU mask", func() {
			for _, node := range workerRTNodes {
				By("Getting tuned.non_isolcpus kernel argument")
				cmdline, err := nodes.ExecCommandOnMachineConfigDaemon(testclient.Client, &node, []string{"cat", "/proc/cmdline"})
				re := regexp.MustCompile(`tuned.non_isolcpus=\S+`)
				nonIsolcpusFullArgument := re.FindString(string(cmdline))
				Expect(nonIsolcpusFullArgument).To(ContainSubstring("tuned.non_isolcpus="))
				nonIsolcpusMask := strings.Split(string(nonIsolcpusFullArgument), "=")[1]
				nonIsolcpusMaskNoDelimiters := strings.Replace(nonIsolcpusMask, ",", "", -1)
				Expect(err).ToNot(HaveOccurred())
				By("executing the command \"cat /sys/devices/virtual/workqueue/cpumask\"")
				workqueueMask, err := nodes.ExecCommandOnMachineConfigDaemon(testclient.Client, &node, []string{"cat", "/sys/devices/virtual/workqueue/cpumask"})
				Expect(err).ToNot(HaveOccurred())
				workqueueMaskTrimmed := strings.TrimSpace(string(workqueueMask))
				workqueueMaskTrimmedNoDelimiters := strings.Replace(workqueueMaskTrimmed, ",", "", -1)
				Expect(strings.TrimLeft(nonIsolcpusMaskNoDelimiters, "0")).Should(Equal(strings.TrimLeft(workqueueMaskTrimmedNoDelimiters, "0")), "workqueueMask is not set to "+workqueueMaskTrimmed)
				By("executing the command \"cat /sys/bus/workqueue/devices/writeback/cpumask\"")
				workqueueWritebackMask, err := nodes.ExecCommandOnMachineConfigDaemon(testclient.Client, &node, []string{"cat", "/sys/bus/workqueue/devices/writeback/cpumask"})
				Expect(err).ToNot(HaveOccurred())
				workqueueWritebackMaskTrimmed := strings.TrimSpace(string(workqueueWritebackMask))
				workqueueWritebackMaskTrimmedNoDelimiters := strings.Replace(workqueueWritebackMaskTrimmed, ",", "", -1)
				Expect(strings.TrimLeft(nonIsolcpusMaskNoDelimiters, "0")).Should(Equal(strings.TrimLeft(workqueueWritebackMaskTrimmedNoDelimiters, "0")), "workqueueMask is not set to "+workqueueWritebackMaskTrimmed)
			}
		})

		// Check /usr/local/bin/pre-boot-tuning.sh existence under worker's rootfs
		const perfRtKernelPrebootTuningScript = "/usr/local/bin/pre-boot-tuning.sh"
		It(perfRtKernelPrebootTuningScript+" should exist on the nodes", func() {
			checkFileExistence(workerRTNodes, perfRtKernelPrebootTuningScript)
		})

		It("Should inject systemd configuration files into initramfs", func() {
			for _, node := range workerRTNodes {
				initramfsImagesPath, err := nodes.ExecCommandOnMachineConfigDaemon(testclient.Client, &node, []string{"find", "/rootfs/boot/ostree/", "-name", "*.img"})
				Expect(err).ToNot(HaveOccurred())
				found := false
				imagesPath := strings.Split(string(initramfsImagesPath), "\n")
				for _, imagePath := range imagesPath[:2] {
					initrd, err := nodes.ExecCommandOnMachineConfigDaemon(testclient.Client, &node,
						[]string{"lsinitrd", strings.TrimSpace(imagePath)})
					Expect(err).ToNot(HaveOccurred())
					initrdString := string(initrd)
					if strings.Contains(initrdString, "'/etc/systemd/system.conf /etc/systemd/system.conf.d/setAffinity.conf'") {
						found = true
						break
					}
				}
				Expect(found).Should(BeTrue())
			}
		})
	})

	Context("FeatureGate - FeatureSet configuration", func() {
		It("FeatureGates with LatencySensitive should exist", func() {
			key := types.NamespacedName{
				Name:      components.FeatureGateLatencySensetiveName,
				Namespace: metav1.NamespaceNone,
			}
			fg := &ocv1.FeatureGate{}
			err := testclient.Client.Get(context.TODO(), key, fg)
			Expect(err).ToNot(HaveOccurred())

			lsStr := string(ocv1.LatencySensitive)
			By("Checking whether FeatureSet is configured as " + lsStr)
			Expect(string(fg.Spec.FeatureSet)).Should(Equal(lsStr), "FeauterSet is not set to "+lsStr)
		})
	})

	// openshift node real time kernel verification
	// (performance-addon-operators/build/assets/tuned/openshift-node-real-time-kernel)
	Context("Tuned kernel parameters", func() {
		It("Should contain configuration injected through openshift-node-real-time-kernel profile", func() {
			sysctlMap := map[string]string{
				"kernel.hung_task_timeout_secs": "600",
				"kernel.nmi_watchdog":           "0",
				"kernel.sched_rt_runtime_us":    "-1",
				"vm.stat_interval":              "10",
				"kernel.timer_migration":        "0",
			}

			key := types.NamespacedName{
				Name:      components.GetComponentName(profileName, components.ProfileNameWorkerRT),
				Namespace: components.NamespaceNodeTuningOperator,
			}
			tuned := &tunedv1.Tuned{}
			err := testclient.Client.Get(context.TODO(), key, tuned)
			Expect(err).ToNot(HaveOccurred(), "cannot find the Cluster Node Tuning Operator object "+key.String())
			validatTunedActiveProfile(workerRTNodes)
			execSysctlOnWorkers(workerRTNodes, sysctlMap)
		})
	})

	// openshift node network latency profile verification
	// (performance-addon-operators/build/assets/tuned/openshift-node-network-latency)
	Context("Network latency parameters adjusted by the Node Tuning Operator", func() {
		It("Should contain configuration injected through the openshift-node-network-latency profile", func() {
			sysctlMap := map[string]string{
				"net.ipv4.tcp_fastopen":           "3",
				"kernel.sched_min_granularity_ns": "10000000",
				"vm.dirty_ratio":                  "10",
				"vm.dirty_background_ratio":       "3",
				"vm.swappiness":                   "10",
				"kernel.sched_migration_cost_ns":  "5000000",
			}
			key := types.NamespacedName{
				Name:      components.ProfileNameNetworkLatency,
				Namespace: components.NamespaceNodeTuningOperator,
			}
			tuned := &tunedv1.Tuned{}
			err := testclient.Client.Get(context.TODO(), key, tuned)
			Expect(err).ToNot(HaveOccurred(), "cannot find the Cluster Node Tuning Operator object "+components.ProfileNameNetworkLatency)
			validatTunedActiveProfile(workerRTNodes)
			execSysctlOnWorkers(workerRTNodes, sysctlMap)
		})
	})
})

func execSysctlOnWorkers(workerNodes []corev1.Node, sysctlMap map[string]string) {
	var err error
	var out []byte
	for _, node := range workerNodes {
		for param, expected := range sysctlMap {
			By(fmt.Sprintf("executing the command \"sysctl -n %s\"", param))
			out, err = nodes.ExecCommandOnMachineConfigDaemon(testclient.Client, &node, []string{"sysctl", "-n", param})
			Expect(err).ToNot(HaveOccurred())
			Expect(strings.TrimSpace(string(out))).Should(Equal(expected), fmt.Sprintf("parameter %s value is not %s.", param, expected))
		}
	}
}

// execute sysctl command inside container in a tuned pod
func validatTunedActiveProfile(nodes []corev1.Node) {
	var err error
	var out []byte
	activeProfileName := components.GetComponentName(profileName, components.ProfileNameWorkerRT)
	for _, node := range nodes {
		tuned := tunedForNode(&node)
		tunedName := tuned.ObjectMeta.Name
		By(fmt.Sprintf("executing the command cat /etc/tuned/active_profile inside the pod %s", tunedName))
		Eventually(func() string {
			out, err = exec.Command("oc", "rsh", "-n", tuned.ObjectMeta.Namespace,
				tunedName, "cat", "/etc/tuned/active_profile").CombinedOutput()
			return strings.TrimSpace(string(out))
		}, testTimeout*time.Second, testPollInterval*time.Second).Should(Equal(activeProfileName),
			fmt.Sprintf("active_profile is not set to %s. %v", activeProfileName, err))
	}
}

// find tuned pod for appropriate node
func tunedForNode(node *corev1.Node) *corev1.Pod {
	listOptions := &client.ListOptions{
		Namespace:     components.NamespaceNodeTuningOperator,
		FieldSelector: fields.SelectorFromSet(fields.Set{"spec.nodeName": node.Name}),
		LabelSelector: labels.SelectorFromSet(labels.Set{"openshift-app": "tuned"}),
	}

	tunedList := &corev1.PodList{}
	Eventually(func() bool {
		if err := testclient.Client.List(context.TODO(), tunedList, listOptions); err != nil {
			return false
		}

		if len(tunedList.Items) == 0 {
			return false
		}
		for _, s := range tunedList.Items[0].Status.ContainerStatuses {
			if s.Ready == false {
				return false
			}
		}
		return true

	}, testTimeout*time.Second, testPollInterval*time.Second).Should(BeTrue(),
		"there should be one tuned daemon per node")

	return &tunedList.Items[0]
}

// Check whether appropriate file exists on the system
func checkFileExistence(workerNodes []corev1.Node, file string) {
	for _, node := range workerNodes {
		By(fmt.Sprintf("Searching for the file %s.Executing the command \"ls /rootfs/%s\"", file, file))
		_, err := nodes.ExecCommandOnMachineConfigDaemon(testclient.Client, &node, []string{"ls", "/rootfs/" + file})
		Expect(err).To(BeNil(), "cannot find the file "+file)
	}
}
