/*
Copyright 2015 The Kubernetes Authors All rights reserved.

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

package executor

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	assertext "github.com/GoogleCloudPlatform/kubernetes/contrib/mesos/pkg/assert"
	"github.com/GoogleCloudPlatform/kubernetes/contrib/mesos/pkg/executor/messages"
	kmruntime "github.com/GoogleCloudPlatform/kubernetes/contrib/mesos/pkg/runtime"
	"github.com/GoogleCloudPlatform/kubernetes/contrib/mesos/pkg/scheduler/podtask"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api/testapi"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client/cache"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/kubelet"
	kconfig "github.com/GoogleCloudPlatform/kubernetes/pkg/kubelet/config"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/kubelet/dockertools"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/runtime"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/watch"

	"github.com/golang/glog"
	bindings "github.com/mesos/mesos-go/executor"
	"github.com/mesos/mesos-go/mesosproto"
	"github.com/mesos/mesos-go/mesosutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

type suicideTracker struct {
	suicideWatcher
	stops  uint32
	resets uint32
	timers uint32
	jumps  *uint32
}

func (t *suicideTracker) Reset(d time.Duration) bool {
	defer func() { t.resets++ }()
	return t.suicideWatcher.Reset(d)
}

func (t *suicideTracker) Stop() bool {
	defer func() { t.stops++ }()
	return t.suicideWatcher.Stop()
}

func (t *suicideTracker) Next(d time.Duration, driver bindings.ExecutorDriver, f jumper) suicideWatcher {
	tracker := &suicideTracker{
		stops:  t.stops,
		resets: t.resets,
		jumps:  t.jumps,
		timers: t.timers + 1,
	}
	jumper := tracker.makeJumper(f)
	tracker.suicideWatcher = t.suicideWatcher.Next(d, driver, jumper)
	return tracker
}

func (t *suicideTracker) makeJumper(_ jumper) jumper {
	return jumper(func(driver bindings.ExecutorDriver, cancel <-chan struct{}) {
		glog.Warningln("jumping?!")
		if t.jumps != nil {
			atomic.AddUint32(t.jumps, 1)
		}
	})
}

func TestSuicide_zeroTimeout(t *testing.T) {
	defer glog.Flush()

	k := New(Config{})
	tracker := &suicideTracker{suicideWatcher: k.suicideWatch}
	k.suicideWatch = tracker

	ch := k.resetSuicideWatch(nil)

	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for reset of suicide watch")
	}
	if tracker.stops != 0 {
		t.Fatalf("expected no stops since suicideWatchTimeout was never set")
	}
	if tracker.resets != 0 {
		t.Fatalf("expected no resets since suicideWatchTimeout was never set")
	}
	if tracker.timers != 0 {
		t.Fatalf("expected no timers since suicideWatchTimeout was never set")
	}
}

func TestSuicide_WithTasks(t *testing.T) {
	defer glog.Flush()

	k := New(Config{
		SuicideTimeout: 50 * time.Millisecond,
	})

	jumps := uint32(0)
	tracker := &suicideTracker{suicideWatcher: k.suicideWatch, jumps: &jumps}
	k.suicideWatch = tracker

	k.tasks["foo"] = &kuberTask{} // prevent suicide attempts from succeeding

	// call reset with a nil timer
	glog.Infoln("resetting suicide watch with 1 task")
	select {
	case <-k.resetSuicideWatch(nil):
		tracker = k.suicideWatch.(*suicideTracker)
		if tracker.stops != 1 {
			t.Fatalf("expected suicide attempt to Stop() since there are registered tasks")
		}
		if tracker.resets != 0 {
			t.Fatalf("expected no resets since")
		}
		if tracker.timers != 0 {
			t.Fatalf("expected no timers since")
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("initial suicide watch setup failed")
	}

	delete(k.tasks, "foo") // zero remaining tasks
	k.suicideTimeout = 1500 * time.Millisecond
	suicideStart := time.Now()

	// reset the suicide watch, which should actually start a timer now
	glog.Infoln("resetting suicide watch with 0 tasks")
	select {
	case <-k.resetSuicideWatch(nil):
		tracker = k.suicideWatch.(*suicideTracker)
		if tracker.stops != 1 {
			t.Fatalf("did not expect suicide attempt to Stop() since there are no registered tasks")
		}
		if tracker.resets != 1 {
			t.Fatalf("expected 1 resets instead of %d", tracker.resets)
		}
		if tracker.timers != 1 {
			t.Fatalf("expected 1 timers instead of %d", tracker.timers)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("2nd suicide watch setup failed")
	}

	k.lock.Lock()
	k.tasks["foo"] = &kuberTask{} // prevent suicide attempts from succeeding
	k.lock.Unlock()

	// reset the suicide watch, which should stop the existing timer
	glog.Infoln("resetting suicide watch with 1 task")
	select {
	case <-k.resetSuicideWatch(nil):
		tracker = k.suicideWatch.(*suicideTracker)
		if tracker.stops != 2 {
			t.Fatalf("expected 2 stops instead of %d since there are registered tasks", tracker.stops)
		}
		if tracker.resets != 1 {
			t.Fatalf("expected 1 resets instead of %d", tracker.resets)
		}
		if tracker.timers != 1 {
			t.Fatalf("expected 1 timers instead of %d", tracker.timers)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("3rd suicide watch setup failed")
	}

	k.lock.Lock()
	delete(k.tasks, "foo") // allow suicide attempts to schedule
	k.lock.Unlock()

	// reset the suicide watch, which should reset a stopped timer
	glog.Infoln("resetting suicide watch with 0 tasks")
	select {
	case <-k.resetSuicideWatch(nil):
		tracker = k.suicideWatch.(*suicideTracker)
		if tracker.stops != 2 {
			t.Fatalf("expected 2 stops instead of %d since there are no registered tasks", tracker.stops)
		}
		if tracker.resets != 2 {
			t.Fatalf("expected 2 resets instead of %d", tracker.resets)
		}
		if tracker.timers != 1 {
			t.Fatalf("expected 1 timers instead of %d", tracker.timers)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("4th suicide watch setup failed")
	}

	sinceWatch := time.Since(suicideStart)
	time.Sleep(3*time.Second - sinceWatch) // give the first timer to misfire (it shouldn't since Stop() was called)

	if j := atomic.LoadUint32(&jumps); j != 1 {
		t.Fatalf("expected 1 jumps instead of %d since stop was called", j)
	} else {
		glog.Infoln("jumps verified") // glog so we get a timestamp
	}
}

// TestExecutorRegister ensures that the executor thinks it is connected
// after Register is called.
func TestExecutorRegister(t *testing.T) {
	mockDriver := &MockExecutorDriver{}
	updates := make(chan interface{}, 1024)
	executor := New(Config{
		Docker:     dockertools.ConnectToDockerOrDie("fake://"),
		Updates:    updates,
		SourceName: "executor_test",
	})

	executor.Init(mockDriver)
	executor.Registered(mockDriver, nil, nil, nil)

	initialPodUpdate := kubelet.PodUpdate{
		Pods:   []*api.Pod{},
		Op:     kubelet.SET,
		Source: executor.sourcename,
	}
	receivedInitialPodUpdate := false
	select {
	case m := <-updates:
		update, ok := m.(kubelet.PodUpdate)
		if ok {
			if reflect.DeepEqual(initialPodUpdate, update) {
				receivedInitialPodUpdate = true
			}
		}
	case <-time.After(time.Second):
	}
	assert.Equal(t, true, receivedInitialPodUpdate,
		"executor should have sent an initial PodUpdate "+
			"to the updates chan upon registration")

	assert.Equal(t, true, executor.isConnected(), "executor should be connected")
	mockDriver.AssertExpectations(t)
}

// TestExecutorDisconnect ensures that the executor thinks that it is not
// connected after a call to Disconnected has occured.
func TestExecutorDisconnect(t *testing.T) {
	mockDriver := &MockExecutorDriver{}
	executor := NewTestKubernetesExecutor()

	executor.Init(mockDriver)
	executor.Registered(mockDriver, nil, nil, nil)
	executor.Disconnected(mockDriver)

	assert.Equal(t, false, executor.isConnected(),
		"executor should not be connected after Disconnected")
	mockDriver.AssertExpectations(t)
}

// TestExecutorReregister ensures that the executor thinks it is connected
// after a connection problem happens, followed by a call to Reregistered.
func TestExecutorReregister(t *testing.T) {
	mockDriver := &MockExecutorDriver{}
	executor := NewTestKubernetesExecutor()

	executor.Init(mockDriver)
	executor.Registered(mockDriver, nil, nil, nil)
	executor.Disconnected(mockDriver)
	executor.Reregistered(mockDriver, nil)

	assert.Equal(t, true, executor.isConnected(), "executor should be connected")
	mockDriver.AssertExpectations(t)
}

type fakeKubelet struct {
	*kubelet.Kubelet
	hostIP net.IP
}

func (kl *fakeKubelet) GetHostIP() (net.IP, error) {
	return kl.hostIP, nil
}

// TestExecutorLaunchAndKillTask ensures that the executor is able to launch
// and kill tasks while properly bookkeping its tasks.
func TestExecutorLaunchAndKillTask(t *testing.T) {
	// create a fake pod watch. We use that below to submit new pods to the scheduler
	podListWatch := NewMockPodsListWatch(api.PodList{})

	// create fake apiserver
	testApiServer := NewTestServer(t, api.NamespaceDefault, &podListWatch.list)
	defer testApiServer.server.Close()

	mockDriver := &MockExecutorDriver{}
	updates := make(chan interface{}, 1024)
	config := Config{
		Docker:  dockertools.ConnectToDockerOrDie("fake://"),
		Updates: updates,
		APIClient: client.NewOrDie(&client.Config{
			Host:    testApiServer.server.URL,
			Version: testapi.Version(),
		}),
		Kubelet: &fakeKubelet{
			Kubelet: &kubelet.Kubelet{},
			hostIP:  net.IPv4(127, 0, 0, 1),
		},
		PodStatusFunc: func(kl KubeletInterface, pod *api.Pod) (*api.PodStatus, error) {
			return &api.PodStatus{
				ContainerStatuses: []api.ContainerStatus{
					{
						Name: "foo",
						State: api.ContainerState{
							Running: &api.ContainerStateRunning{},
						},
					},
				},
				Phase: api.PodRunning,
			}, nil
		},
	}
	executor := New(config)

	executor.Init(mockDriver)
	executor.Registered(mockDriver, nil, nil, nil)

	select {
	case <-updates:
	case <-time.After(time.Second):
		t.Fatalf("Executor should send an intial update on Registration")
	}

	pod := NewTestPod(1)
	podTask, err := podtask.New(api.NewDefaultContext(), "",
		*pod, &mesosproto.ExecutorInfo{})
	assert.Equal(t, nil, err, "must be able to create a task from a pod")

	taskInfo := podTask.BuildTaskInfo()
	data, err := testapi.Codec().Encode(pod)
	assert.Equal(t, nil, err, "must be able to encode a pod's spec data")
	taskInfo.Data = data
	var statusUpdateCalls sync.WaitGroup
	statusUpdateDone := func(_ mock.Arguments) { statusUpdateCalls.Done() }

	statusUpdateCalls.Add(1)
	mockDriver.On(
		"SendStatusUpdate",
		mesosproto.TaskState_TASK_STARTING,
	).Return(mesosproto.Status_DRIVER_RUNNING, nil).Run(statusUpdateDone).Once()

	statusUpdateCalls.Add(1)
	mockDriver.On(
		"SendStatusUpdate",
		mesosproto.TaskState_TASK_RUNNING,
	).Return(mesosproto.Status_DRIVER_RUNNING, nil).Run(statusUpdateDone).Once()

	executor.LaunchTask(mockDriver, taskInfo)

	assertext.EventuallyTrue(t, 5*time.Second, func() bool {
		executor.lock.Lock()
		defer executor.lock.Unlock()
		return len(executor.tasks) == 1 && len(executor.pods) == 1
	}, "executor must be able to create a task and a pod")

	gotPodUpdate := false
	select {
	case m := <-updates:
		update, ok := m.(kubelet.PodUpdate)
		if ok && len(update.Pods) == 1 {
			gotPodUpdate = true
		}
	case <-time.After(time.Second):
	}
	assert.Equal(t, true, gotPodUpdate,
		"the executor should send an update about a new pod to "+
			"the updates chan when creating a new one.")

	// Allow some time for asynchronous requests to the driver.
	finished := kmruntime.After(statusUpdateCalls.Wait)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for status update calls to finish")
	}

	statusUpdateCalls.Add(1)
	mockDriver.On(
		"SendStatusUpdate",
		mesosproto.TaskState_TASK_KILLED,
	).Return(mesosproto.Status_DRIVER_RUNNING, nil).Run(statusUpdateDone).Once()

	executor.KillTask(mockDriver, taskInfo.TaskId)

	assertext.EventuallyTrue(t, 5*time.Second, func() bool {
		executor.lock.Lock()
		defer executor.lock.Unlock()
		return len(executor.tasks) == 0 && len(executor.pods) == 0
	}, "executor must be able to kill a created task and pod")

	// Allow some time for asynchronous requests to the driver.
	finished = kmruntime.After(statusUpdateCalls.Wait)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for status update calls to finish")
	}
	mockDriver.AssertExpectations(t)
}

// TestExecutorStaticPods test that the ExecutorInfo.data is parsed
// as a zip archive with pod definitions.
func TestExecutorStaticPods(t *testing.T) {
	// create some zip with static pod definition
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	createStaticPodFile := func(fileName, id, name string) {
		w, err := zw.Create(fileName)
		assert.NoError(t, err)
		spod := `{
	"apiVersion": "v1beta3",
	"name": "%v",
	"kind": "Pod",
	"metadata": {
		"labels": { "name": "foo", "cluster": "bar" }
	},
	"spec": {
		"containers": [{
			"name": "%v",
			"image": "library/nginx",
			"ports": [{ "containerPort": 80, "name": "http" }],
			"livenessProbe": {
				"enabled": true,
				"type": "http",
				"initialDelaySeconds": 30,
				"httpGet": { "path": "/", "port": "80" }
			}
		}]
	}
	}`
		_, err = w.Write([]byte(fmt.Sprintf(spod, id, name)))
		assert.NoError(t, err)
	}
	createStaticPodFile("spod.json", "spod-id-01", "spod-01")
	createStaticPodFile("spod2.json", "spod-id-02", "spod-02")
	createStaticPodFile("dir/spod.json", "spod-id-03", "spod-03") // same file name as first one to check for overwriting

	expectedStaticPodsNum := 2 // subdirectories are ignored by FileSource, hence only 2

	err := zw.Close()
	assert.NoError(t, err)

	// create fake apiserver
	testApiServer := NewTestServer(t, api.NamespaceDefault, nil)
	defer testApiServer.server.Close()

	// temporary directory which is normally located in the executor sandbox
	staticPodsConfigPath, err := ioutil.TempDir("/tmp", "executor-k8sm-archive")
	assert.NoError(t, err)
	defer os.RemoveAll(staticPodsConfigPath)

	mockDriver := &MockExecutorDriver{}
	updates := make(chan interface{}, 1024)
	config := Config{
		Docker:  dockertools.ConnectToDockerOrDie("fake://"),
		Updates: make(chan interface{}, 1), // allow kube-executor source to proceed past init
		APIClient: client.NewOrDie(&client.Config{
			Host:    testApiServer.server.URL,
			Version: testapi.Version(),
		}),
		Kubelet: &kubelet.Kubelet{},
		PodStatusFunc: func(kl KubeletInterface, pod *api.Pod) (*api.PodStatus, error) {
			return &api.PodStatus{
				ContainerStatuses: []api.ContainerStatus{
					{
						Name: "foo",
						State: api.ContainerState{
							Running: &api.ContainerStateRunning{},
						},
					},
				},
				Phase: api.PodRunning,
			}, nil
		},
		StaticPodsConfigPath: staticPodsConfigPath,
	}
	executor := New(config)
	hostname := "h1"
	go executor.InitializeStaticPodsSource(func() {
		kconfig.NewSourceFile(staticPodsConfigPath, hostname, 1*time.Second, updates)
	})

	// create ExecutorInfo with static pod zip in data field
	executorInfo := mesosutil.NewExecutorInfo(
		mesosutil.NewExecutorID("ex1"),
		mesosutil.NewCommandInfo("k8sm-executor"),
	)
	executorInfo.Data = buf.Bytes()

	// start the executor with the static pod data
	executor.Init(mockDriver)
	executor.Registered(mockDriver, executorInfo, nil, nil)

	// wait for static pod to start
	seenPods := map[string]struct{}{}
	timeout := time.After(time.Second)
	defer mockDriver.AssertExpectations(t)
	for {
		// filter by PodUpdate type
		select {
		case <-timeout:
			t.Fatalf("Executor should send pod updates for %v pods, only saw %v", expectedStaticPodsNum, len(seenPods))
		case update, ok := <-updates:
			if !ok {
				return
			}
			podUpdate, ok := update.(kubelet.PodUpdate)
			if !ok {
				continue
			}
			for _, pod := range podUpdate.Pods {
				seenPods[pod.Name] = struct{}{}
			}
			if len(seenPods) == expectedStaticPodsNum {
				return
			}
		}
	}
}

// TestExecutorFrameworkMessage ensures that the executor is able to
// handle messages from the framework, specifically about lost tasks
// and Kamikaze.  When a task is lost, the executor needs to clean up
// its state.  When a Kamikaze message is received, the executor should
// attempt suicide.
func TestExecutorFrameworkMessage(t *testing.T) {
	// create fake apiserver
	podListWatch := NewMockPodsListWatch(api.PodList{})
	testApiServer := NewTestServer(t, api.NamespaceDefault, &podListWatch.list)
	defer testApiServer.server.Close()

	// create and start executor
	mockDriver := &MockExecutorDriver{}
	kubeletFinished := make(chan struct{})
	config := Config{
		Docker:  dockertools.ConnectToDockerOrDie("fake://"),
		Updates: make(chan interface{}, 1024),
		APIClient: client.NewOrDie(&client.Config{
			Host:    testApiServer.server.URL,
			Version: testapi.Version(),
		}),
		Kubelet: &fakeKubelet{
			Kubelet: &kubelet.Kubelet{},
			hostIP:  net.IPv4(127, 0, 0, 1),
		},
		PodStatusFunc: func(kl KubeletInterface, pod *api.Pod) (*api.PodStatus, error) {
			return &api.PodStatus{
				ContainerStatuses: []api.ContainerStatus{
					{
						Name: "foo",
						State: api.ContainerState{
							Running: &api.ContainerStateRunning{},
						},
					},
				},
				Phase: api.PodRunning,
			}, nil
		},
		ShutdownAlert: func() {
			close(kubeletFinished)
		},
		KubeletFinished: kubeletFinished,
	}
	executor := New(config)

	executor.Init(mockDriver)
	executor.Registered(mockDriver, nil, nil, nil)

	executor.FrameworkMessage(mockDriver, "test framework message")

	// set up a pod to then lose
	pod := NewTestPod(1)
	podTask, _ := podtask.New(api.NewDefaultContext(), "foo",
		*pod, &mesosproto.ExecutorInfo{})

	taskInfo := podTask.BuildTaskInfo()
	data, _ := testapi.Codec().Encode(pod)
	taskInfo.Data = data

	mockDriver.On(
		"SendStatusUpdate",
		mesosproto.TaskState_TASK_STARTING,
	).Return(mesosproto.Status_DRIVER_RUNNING, nil).Once()

	called := make(chan struct{})
	mockDriver.On(
		"SendStatusUpdate",
		mesosproto.TaskState_TASK_RUNNING,
	).Return(mesosproto.Status_DRIVER_RUNNING, nil).Run(func(_ mock.Arguments) { close(called) }).Once()

	executor.LaunchTask(mockDriver, taskInfo)

	// waiting until the pod is really running b/c otherwise a TASK_FAILED could be
	// triggered by the asynchronously running  _launchTask, __launchTask methods
	// when removing the task from k.tasks through the "task-lost:foo" message below.
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for SendStatusUpdate for the running task")
	}

	// send task-lost message for it
	called = make(chan struct{})
	mockDriver.On(
		"SendStatusUpdate",
		mesosproto.TaskState_TASK_LOST,
	).Return(mesosproto.Status_DRIVER_RUNNING, nil).Run(func(_ mock.Arguments) { close(called) }).Once()

	executor.FrameworkMessage(mockDriver, "task-lost:foo")
	assertext.EventuallyTrue(t, 5*time.Second, func() bool {
		executor.lock.Lock()
		defer executor.lock.Unlock()
		return len(executor.tasks) == 0 && len(executor.pods) == 0
	}, "executor must be able to kill a created task and pod")

	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for SendStatusUpdate")
	}

	mockDriver.On("Stop").Return(mesosproto.Status_DRIVER_STOPPED, nil).Once()

	executor.FrameworkMessage(mockDriver, messages.Kamikaze)
	assert.Equal(t, true, executor.isDone(),
		"executor should have shut down after receiving a Kamikaze message")

	mockDriver.AssertExpectations(t)
}

// Create a pod with a given index, requiring one port
func NewTestPod(i int) *api.Pod {
	name := fmt.Sprintf("pod%d", i)
	return &api.Pod{
		TypeMeta: api.TypeMeta{APIVersion: testapi.Version()},
		ObjectMeta: api.ObjectMeta{
			Name:      name,
			Namespace: api.NamespaceDefault,
			SelfLink:  testapi.SelfLink("pods", string(i)),
		},
		Spec: api.PodSpec{
			Containers: []api.Container{
				{
					Name: "foo",
					Ports: []api.ContainerPort{
						{
							ContainerPort: 8000 + i,
							Protocol:      api.ProtocolTCP,
						},
					},
				},
			},
		},
		Status: api.PodStatus{
			Conditions: []api.PodCondition{
				{
					Type:   api.PodReady,
					Status: api.ConditionTrue,
				},
			},
		},
	}
}

// Create mock of pods ListWatch, usually listening on the apiserver pods watch endpoint
type MockPodsListWatch struct {
	ListWatch   cache.ListWatch
	fakeWatcher *watch.FakeWatcher
	list        api.PodList
}

// A apiserver mock which partially mocks the pods API
type TestServer struct {
	server *httptest.Server
	Stats  map[string]uint
	lock   sync.Mutex
}

func NewTestServer(t *testing.T, namespace string, pods *api.PodList) *TestServer {
	ts := TestServer{
		Stats: map[string]uint{},
	}
	mux := http.NewServeMux()

	mux.HandleFunc(testapi.ResourcePath("bindings", namespace, ""), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ts.server = httptest.NewServer(mux)
	return &ts
}

func NewMockPodsListWatch(initialPodList api.PodList) *MockPodsListWatch {
	lw := MockPodsListWatch{
		fakeWatcher: watch.NewFake(),
		list:        initialPodList,
	}
	lw.ListWatch = cache.ListWatch{
		WatchFunc: func(resourceVersion string) (watch.Interface, error) {
			return lw.fakeWatcher, nil
		},
		ListFunc: func() (runtime.Object, error) {
			return &lw.list, nil
		},
	}
	return &lw
}

// TestExecutorShutdown ensures that the executor properly shuts down
// when Shutdown is called.
func TestExecutorShutdown(t *testing.T) {
	mockDriver := &MockExecutorDriver{}
	kubeletFinished := make(chan struct{})
	var exitCalled int32 = 0
	config := Config{
		Docker:  dockertools.ConnectToDockerOrDie("fake://"),
		Updates: make(chan interface{}, 1024),
		ShutdownAlert: func() {
			close(kubeletFinished)
		},
		KubeletFinished: kubeletFinished,
		ExitFunc: func(_ int) {
			atomic.AddInt32(&exitCalled, 1)
		},
	}
	executor := New(config)

	executor.Init(mockDriver)
	executor.Registered(mockDriver, nil, nil, nil)

	mockDriver.On("Stop").Return(mesosproto.Status_DRIVER_STOPPED, nil).Once()

	executor.Shutdown(mockDriver)

	assert.Equal(t, false, executor.isConnected(),
		"executor should not be connected after Shutdown")
	assert.Equal(t, true, executor.isDone(),
		"executor should be in Done state after Shutdown")

	select {
	case <-executor.Done():
	default:
		t.Fatal("done channel should be closed after shutdown")
	}

	assert.Equal(t, true, atomic.LoadInt32(&exitCalled) > 0,
		"the executor should call its ExitFunc when it is ready to close down")

	mockDriver.AssertExpectations(t)
}

func TestExecutorsendFrameworkMessage(t *testing.T) {
	mockDriver := &MockExecutorDriver{}
	executor := NewTestKubernetesExecutor()

	executor.Init(mockDriver)
	executor.Registered(mockDriver, nil, nil, nil)

	called := make(chan struct{})
	mockDriver.On(
		"SendFrameworkMessage",
		"foo bar baz",
	).Return(mesosproto.Status_DRIVER_RUNNING, nil).Run(func(_ mock.Arguments) { close(called) }).Once()
	executor.sendFrameworkMessage(mockDriver, "foo bar baz")

	// guard against data race in mock driver between AssertExpectations and Called
	select {
	case <-called: // expected
	case <-time.After(5 * time.Second):
		t.Fatalf("expected call to SendFrameworkMessage")
	}
	mockDriver.AssertExpectations(t)
}
