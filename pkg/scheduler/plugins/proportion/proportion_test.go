/*
Copyright 2022 The Kubernetes Authors.
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

package proportion

import (
	"io/ioutil"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/util/workqueue"
	"volcano.sh/volcano/pkg/scheduler/actions/allocate"

	"github.com/agiledragon/gomonkey/v2"
	apiv1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	"volcano.sh/volcano/cmd/scheduler/app/options"
	"volcano.sh/volcano/pkg/scheduler/api"
	"volcano.sh/volcano/pkg/scheduler/cache"
	"volcano.sh/volcano/pkg/scheduler/conf"
	"volcano.sh/volcano/pkg/scheduler/framework"
	"volcano.sh/volcano/pkg/scheduler/plugins/gang"
	"volcano.sh/volcano/pkg/scheduler/plugins/priority"
	"volcano.sh/volcano/pkg/scheduler/util"
)

func getWorkerAffinity() *apiv1.Affinity {
	return &apiv1.Affinity{
		PodAntiAffinity: &apiv1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []apiv1.PodAffinityTerm{
				{
					LabelSelector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "role",
								Operator: "In",
								Values:   []string{"worker"},
							},
						},
					},
					TopologyKey: "kubernetes.io/hostname",
				},
			},
		},
	}
}

func getLocalMetrics() int {
	var data int

	url := "http://127.0.0.1:8081/metrics"
	method := "GET"

	client := &http.Client{}
	req, err := http.NewRequest(method, url, nil)

	if err != nil {
		return data
	}
	req.Header.Add("Authorization", "8cbdb37a-b880-4f2e-844c-e420858ea7eb")

	res, err := client.Do(req)
	if err != nil {
		return data
	}
	defer res.Body.Close()

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return data
	}

	split := strings.Split(string(body), "\n")
	for _, v := range split {
		if !strings.Contains(v, "#") && (strings.Contains(v, "volcano_queue_allocated_memory_bytes") || strings.Contains(v, "volcano_queue_allocated_milli_cpu")) {
			data, _ = strconv.Atoi(strings.Split(v, " ")[1])
		}
	}

	return data
}

func TestProportionPanic(t *testing.T) {
	c := make(chan bool, 1)
	var tmp *cache.SchedulerCache
	patches := gomonkey.ApplyMethod(reflect.TypeOf(tmp), "AddBindTask", func(scCache *cache.SchedulerCache, task *api.TaskInfo) error {
		scCache.Binder.Bind(nil, []*api.TaskInfo{task})
		return nil
	})
	defer patches.Reset()

	framework.RegisterPluginBuilder(PluginName, New)
	framework.RegisterPluginBuilder(gang.PluginName, gang.New)
	framework.RegisterPluginBuilder(priority.PluginName, priority.New)
	options.ServerOpts = options.NewServerOption()
	//defer framework.CleanupPluginBuilders()

	// Running pods
	w1 := util.BuildPod("ns1", "worker-1", "", apiv1.PodRunning, util.BuildResourceList("40", "1k"), "pg1", map[string]string{"role": "worker"}, map[string]string{"selector": "worker"})
	w2 := util.BuildPod("ns2", "worker-2", "", apiv1.PodRunning, util.BuildResourceList("20", "1k"), "pg2", map[string]string{"role": "worker"}, map[string]string{"selector": "worker"})
	w3 := util.BuildPod("ns2", "worker-3", "", apiv1.PodPending, util.BuildResourceList("20", "1k"), "pg3", map[string]string{"role": "worker"}, map[string]string{"selector": "worker"})
	////w1.Spec.Affinity = getWorkerAffinity()

	// nodes
	n1 := util.BuildNode("node1", util.BuildResourceList("50", "4k"), map[string]string{"selector": "worker"})
	n2 := util.BuildNode("node2", util.BuildResourceList("50", "3k"), map[string]string{})
	n1.Status.Allocatable["pods"] = resource.MustParse("15")
	n2.Status.Allocatable["pods"] = resource.MustParse("15")
	n1.Labels["kubernetes.io/hostname"] = "node1"
	n2.Labels["kubernetes.io/hostname"] = "node2"

	// priority
	p1 := &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: "p1"}, Value: 1}
	// podgroup
	pg1 := &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1",
			Name:      "pg1",
		},
		Spec: schedulingv1beta1.PodGroupSpec{
			Queue:             "q1",
			MinMember:         int32(2),
			PriorityClassName: p1.Name,
			MinResources: &v1.ResourceList{
				v1.ResourceName("cpu"): resource.MustParse("40"),
			},
		},
	}
	pg2 := &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns2",
			Name:      "pg2",
		},
		Spec: schedulingv1beta1.PodGroupSpec{
			Queue:             "q2",
			MinMember:         int32(1),
			PriorityClassName: p1.Name,
			MinResources: &v1.ResourceList{
				v1.ResourceName("cpu"): resource.MustParse("20"),
			},
		},
	}
	pg3 := &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns2",
			Name:      "pg3",
		},
		Spec: schedulingv1beta1.PodGroupSpec{
			Queue:             "q3",
			MinMember:         int32(1),
			PriorityClassName: p1.Name,
			MinResources: &v1.ResourceList{
				v1.ResourceName("cpu"): resource.MustParse("20"),
			},
		},
	}

	// queue
	queue1 := &schedulingv1beta1.Queue{
		ObjectMeta: metav1.ObjectMeta{
			Name: "q1",
		},
		Spec: schedulingv1beta1.QueueSpec{
			Weight: 1,
			Capability: v1.ResourceList{
				v1.ResourceName("cpu"): resource.MustParse("80"),
			},
			Guarantee: schedulingv1beta1.Guarantee{
				v1.ResourceList{
					v1.ResourceName("cpu"): resource.MustParse("80"),
				},
			},
		},
	}
	queue2 := &schedulingv1beta1.Queue{
		ObjectMeta: metav1.ObjectMeta{
			Name: "q2",
		},
		Spec: schedulingv1beta1.QueueSpec{
			Weight: 1,
			Capability: v1.ResourceList{
				v1.ResourceName("cpu"): resource.MustParse("20"),
			},
			Guarantee: schedulingv1beta1.Guarantee{
				v1.ResourceList{
					v1.ResourceName("cpu"): resource.MustParse("0"),
				},
			},
		},
	}
	queue3 := &schedulingv1beta1.Queue{
		ObjectMeta: metav1.ObjectMeta{
			Name: "q3",
		},
		Spec: schedulingv1beta1.QueueSpec{
			Weight: 1,
			Capability: v1.ResourceList{
				v1.ResourceName("cpu"): resource.MustParse("20"),
			},
			Guarantee: schedulingv1beta1.Guarantee{
				v1.ResourceList{
					v1.ResourceName("cpu"): resource.MustParse("0"),
				},
			},
		},
	}

	// tests
	tests := []struct {
		name     string
		pods     []*apiv1.Pod
		nodes    []*apiv1.Node
		pcs      []*schedulingv1.PriorityClass
		pgs      []*schedulingv1beta1.PodGroup
		expected map[string]string
	}{
		{
			name:  "pod-panic",
			pods:  []*apiv1.Pod{w1, w2, w3},
			nodes: []*apiv1.Node{n1, n2},
			pcs:   []*schedulingv1.PriorityClass{p1},
			pgs:   []*schedulingv1beta1.PodGroup{pg1, pg2, pg3},
			expected: map[string]string{ // podKey -> node
				"ns1/worker-3": "node1",
			},
		},
	}

	for _, test := range tests {
		// initialize schedulerCache
		binder := &util.FakeBinder{
			Binds:   map[string]string{},
			Channel: make(chan string),
		}
		recorder := record.NewFakeRecorder(100)
		go func() {
			for {
				event := <-recorder.Events
				t.Logf("%s: [Event] %s", test.name, event)
			}
		}()
		schedulerCache := &cache.SchedulerCache{
			Nodes:           make(map[string]*api.NodeInfo),
			Jobs:            make(map[api.JobID]*api.JobInfo),
			PriorityClasses: make(map[string]*schedulingv1.PriorityClass),
			Queues:          make(map[api.QueueID]*api.QueueInfo),
			Binder:          binder,
			StatusUpdater:   &util.FakeStatusUpdater{},
			VolumeBinder:    &util.FakeVolumeBinder{},
			Recorder:        recorder,
		}
		// deletedJobs to DeletedJobs
		schedulerCache.DeletedJobs = workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

		for _, node := range test.nodes {
			schedulerCache.AddNode(node)
		}
		for _, pod := range test.pods {
			schedulerCache.AddPod(pod)
		}
		for _, pc := range test.pcs {
			schedulerCache.PriorityClasses[pc.Name] = pc
		}
		for _, pg := range test.pgs {
			pg.Status = schedulingv1beta1.PodGroupStatus{
				Phase: schedulingv1beta1.PodGroupInqueue,
			}
			schedulerCache.AddPodGroupV1beta1(pg)
		}
		schedulerCache.AddQueueV1beta1(queue1)
		schedulerCache.AddQueueV1beta1(queue2)
		schedulerCache.AddQueueV1beta1(queue3)
		//for _, info := range schedulerCache.Jobs {
		//	t.Log(fmt.Printf("info %v", info))
		//}
		// session
		trueValue := true

		num := 1
		// proportion
		go func() {
			for {
				select {
				default:
					ssn := framework.OpenSession(schedulerCache, []conf.Tier{
						{
							Plugins: []conf.PluginOption{
								{
									Name:             PluginName,
									EnabledPredicate: &trueValue,
								},
								{
									Name:                gang.PluginName,
									EnabledJobReady:     &trueValue,
									EnabledJobPipelined: &trueValue,
								},
								{
									Name:            priority.PluginName,
									EnabledJobOrder: &trueValue,
								},
							},
						},
					}, nil)

					allocator := allocate.New()
					allocator.Execute(ssn)
					framework.CloseSession(ssn)
					time.Sleep(time.Second * 3)
					if num == 1 {
						metrics := getLocalMetrics()
						if metrics == 12000 {
							t.Logf("init queue_allocated metrics is ok,%v", metrics)
						}
						//schedulerCache.DeletePodGroupV1beta1(pg1)
					} else {
						metrics := getLocalMetrics()
						if metrics != 0 {
							t.Errorf("after delete vcjob pg2, queue_allocated metrics is fail,%v", metrics)
							c <- false
							return
						} else {
							t.Logf("after delete vcjob pg2, queue_allocated metrics is ok,%v", metrics)
							c <- true
						}
					}
					num++
				}
			}
		}()

		go func() {
			http.Handle("/metrics", promhttp.Handler())
			err := http.ListenAndServe(":8081", nil)
			if err != nil {
				t.Errorf("ListenAndServe() err = %v", err.Error())
			}
		}()

		for {
			select {
			case res := <-c:
				if !res {
					t.Error("TestProportion failed")
				} else {
					t.Log("TestProportion successful")
				}
				return
			}

		}
	}
}
