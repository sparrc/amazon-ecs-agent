//go:build unit
// +build unit

// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package eventhandler

import (
	"container/list"
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aws/amazon-ecs-agent/agent/api"
	apicontainer "github.com/aws/amazon-ecs-agent/agent/api/container"
	apitask "github.com/aws/amazon-ecs-agent/agent/api/task"
	"github.com/aws/amazon-ecs-agent/agent/data"
	"github.com/aws/amazon-ecs-agent/agent/engine/dockerstate"
	mock_dockerstate "github.com/aws/amazon-ecs-agent/agent/engine/dockerstate/mocks"
	"github.com/aws/amazon-ecs-agent/agent/statechange"
	"github.com/aws/amazon-ecs-agent/agent/utils"
	"github.com/aws/amazon-ecs-agent/ecs-agent/api/attachment"
	apicontainerstatus "github.com/aws/amazon-ecs-agent/ecs-agent/api/container/status"
	"github.com/aws/amazon-ecs-agent/ecs-agent/api/ecs"
	mock_ecs "github.com/aws/amazon-ecs-agent/ecs-agent/api/ecs/mocks"
	apierrors "github.com/aws/amazon-ecs-agent/ecs-agent/api/errors"
	apitaskstatus "github.com/aws/amazon-ecs-agent/ecs-agent/api/task/status"
	ni "github.com/aws/amazon-ecs-agent/ecs-agent/netlib/model/networkinterface"
	mock_retry "github.com/aws/amazon-ecs-agent/ecs-agent/utils/retry/mock"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"
	"github.com/golang/mock/gomock"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
)

const taskARN = "taskarn"

func TestSendsEventsOneContainer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	client := mock_ecs.NewMockECSClient(ctrl)

	ctx, cancel := context.WithCancel(context.Background())
	handler := NewTaskHandler(ctx, data.NewNoopClient(), dockerstate.NewTaskEngineState(), client)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)

	// Trivial: one container, no errors
	contEvent1 := containerEvent(taskARN)
	contEvent2 := containerEvent(taskARN)
	taskEvent2 := taskEvent(taskARN)

	client.EXPECT().SubmitTaskStateChange(gomock.Any()).Do(func(change ecs.TaskStateChange) {
		assert.Equal(t, 2, len(change.Containers))
		assert.Equal(t, taskARN, change.TaskARN)
		wg.Done()
	})

	handler.AddStateChangeEvent(contEvent1, client)
	handler.AddStateChangeEvent(contEvent2, client)
	handler.AddStateChangeEvent(taskEvent2, client)

	wg.Wait()
}

func TestSendsEventsOneEventRetries(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	client := mock_ecs.NewMockECSClient(ctrl)

	ctx, cancel := context.WithCancel(context.Background())
	handler := NewTaskHandler(ctx, data.NewNoopClient(), dockerstate.NewTaskEngineState(), client)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)

	retriable := apierrors.NewRetriableError(apierrors.NewRetriable(true), errors.New("test"))
	taskEvent := taskEvent(taskARN)

	gomock.InOrder(
		client.EXPECT().SubmitTaskStateChange(gomock.Any()).Return(retriable).Do(func(interface{}) { wg.Done() }),
		client.EXPECT().SubmitTaskStateChange(gomock.Any()).Return(nil).Do(func(interface{}) { wg.Done() }),
	)

	handler.AddStateChangeEvent(taskEvent, client)

	wg.Wait()
}

func TestSendsEventsInvalidParametersEventsRemoved(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	client := mock_ecs.NewMockECSClient(ctrl)

	ctx, cancel := context.WithCancel(context.Background())
	handler := NewTaskHandler(ctx, data.NewNoopClient(), dockerstate.NewTaskEngineState(), client)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)

	taskEvent := taskEvent(taskARN)

	client.EXPECT().SubmitTaskStateChange(gomock.Any()).Do(func(interface{}) {
		assert.Equal(t, 1, handler.tasksToEvents[taskARN].events.Len())
		wg.Done()
	}).Return(&smithy.GenericAPIError{
		Code:    apierrors.ErrCodeInvalidParameterException,
		Message: "",
	})

	handler.AddStateChangeEvent(taskEvent, client)

	wg.Wait()
	// Require the lock to wait for submitFirstEvent to be finished
	handler.tasksToEvents[taskARN].lock.Lock()
	assert.Equal(t, 0, handler.tasksToEvents[taskARN].events.Len())
	handler.tasksToEvents[taskARN].lock.Unlock()
}

func TestSendsEventsConcurrentLimit(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	client := mock_ecs.NewMockECSClient(ctrl)

	ctx, cancel := context.WithCancel(context.Background())
	handler := NewTaskHandler(ctx, data.NewNoopClient(), dockerstate.NewTaskEngineState(), client)
	defer cancel()

	completeStateChange := make(chan bool, concurrentEventCalls+1)
	var wg sync.WaitGroup

	client.EXPECT().SubmitTaskStateChange(gomock.Any()).Times(concurrentEventCalls + 1).Do(func(interface{}) {
		wg.Done()
		<-completeStateChange
	})

	// Test concurrency; ensure it doesn't attempt to send more than
	// concurrentEventCalls at once
	wg.Add(concurrentEventCalls)

	// Put on N+1 events
	for i := 0; i < concurrentEventCalls+1; i++ {
		handler.AddStateChangeEvent(taskEvent("concurrent_"+strconv.Itoa(i)), client)
	}
	wg.Wait()

	// accept a single change event
	wg.Add(1)
	completeStateChange <- true
	wg.Wait()

	// ensure the remaining requests are completed
	for i := 0; i < concurrentEventCalls; i++ {
		completeStateChange <- true
	}
}

func TestSendsEventsContainerDifferences(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	client := mock_ecs.NewMockECSClient(ctrl)

	ctx, cancel := context.WithCancel(context.Background())
	handler := NewTaskHandler(ctx, data.NewNoopClient(), dockerstate.NewTaskEngineState(), client)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)

	// Test container event replacement doesn't happen
	contEvent1 := containerEvent(taskARN)
	contEvent2 := containerEventStopped(taskARN)
	taskEvent := taskEvent(taskARN)

	client.EXPECT().SubmitTaskStateChange(gomock.Any()).Do(func(change ecs.TaskStateChange) {
		assert.Equal(t, taskARN, change.TaskARN)
		assert.Equal(t, apicontainerstatus.ContainerRunning.String(), aws.ToString(change.Containers[0].Status))
		assert.Equal(t, apicontainerstatus.ContainerStopped.String(), aws.ToString(change.Containers[1].Status))
		wg.Done()
	})

	handler.AddStateChangeEvent(contEvent1, client)
	handler.AddStateChangeEvent(contEvent2, client)
	handler.AddStateChangeEvent(taskEvent, client)

	wg.Wait()
}

func TestSendsEventsTaskDifferences(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	client := mock_ecs.NewMockECSClient(ctrl)
	dataClient := data.NewNoopClient()

	ctx, cancel := context.WithCancel(context.Background())
	handler := NewTaskHandler(ctx, dataClient, dockerstate.NewTaskEngineState(), client)
	defer cancel()

	taskARNA := "taskarnA"
	taskARNB := "taskarnB"

	var wg sync.WaitGroup
	wg.Add(2)

	var wgAddEvent sync.WaitGroup
	wgAddEvent.Add(1)

	// Test task event replacement doesn't happen
	taskEventA := taskEvent(taskARNA)
	contEventA1 := containerEvent(taskARNA)

	contEventB1 := containerEvent(taskARNB)
	contEventB2 := containerEventStopped(taskARNB)
	taskEventB := taskEventStopped(taskARNB)

	client.EXPECT().SubmitTaskStateChange(gomock.Any()).Do(func(change ecs.TaskStateChange) {
		assert.Equal(t, taskARNA, change.TaskARN)
		wgAddEvent.Done()
		wg.Done()
	})

	client.EXPECT().SubmitTaskStateChange(gomock.Any()).Do(func(change ecs.TaskStateChange) {
		assert.Equal(t, taskARNB, change.TaskARN)
		wg.Done()
	})

	handler.AddStateChangeEvent(contEventB1, client)
	handler.AddStateChangeEvent(contEventA1, client)
	handler.AddStateChangeEvent(contEventB2, client)

	handler.AddStateChangeEvent(taskEventA, client)
	wgAddEvent.Wait()

	handler.AddStateChangeEvent(taskEventB, client)

	wg.Wait()
}

func TestSendsEventsDedupe(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	client := mock_ecs.NewMockECSClient(ctrl)

	ctx, cancel := context.WithCancel(context.Background())
	handler := NewTaskHandler(ctx, data.NewNoopClient(), dockerstate.NewTaskEngineState(), client)
	defer cancel()

	taskARNA := "taskarnA"
	taskARNB := "taskarnB"

	var wg sync.WaitGroup
	wg.Add(1)

	// Verify that a task doesn't get sent if we already have 'sent' it
	task1 := taskEvent(taskARNA)
	task1.(api.TaskStateChange).Task.SetSentStatus(apitaskstatus.TaskRunning)
	cont1 := containerEvent(taskARNA)
	cont1.(api.ContainerStateChange).Container.SetSentStatus(apicontainerstatus.ContainerRunning)

	handler.AddStateChangeEvent(cont1, client)
	handler.AddStateChangeEvent(task1, client)

	task2 := taskEvent(taskARNB)
	task2.(api.TaskStateChange).Task.SetSentStatus(apitaskstatus.TaskStatusNone)
	cont2 := containerEvent(taskARNB)
	cont2.(api.ContainerStateChange).Container.SetSentStatus(apicontainerstatus.ContainerRunning)

	// Expect to send a task status but not a container status
	client.EXPECT().SubmitTaskStateChange(gomock.Any()).Do(func(change ecs.TaskStateChange) {
		assert.Equal(t, 1, len(change.Containers))
		assert.Equal(t, taskARNB, change.TaskARN)
		wg.Done()
	})

	handler.AddStateChangeEvent(cont2, client)
	handler.AddStateChangeEvent(task2, client)

	wg.Wait()
}

// TestCleanupTaskEventAfterSubmit tests the map of task event is removed after
// calling submittaskstatechange
func TestCleanupTaskEventAfterSubmit(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	client := mock_ecs.NewMockECSClient(ctrl)

	ctx, cancel := context.WithCancel(context.Background())
	handler := NewTaskHandler(ctx, data.NewNoopClient(), dockerstate.NewTaskEngineState(), client)
	defer cancel()

	taskARN2 := "taskarn2"

	var wg sync.WaitGroup
	wg.Add(3)

	taskEvent1 := taskEvent(taskARN)
	taskEvent2 := taskEvent(taskARN)
	taskEvent3 := taskEvent(taskARN2)

	client.EXPECT().SubmitTaskStateChange(gomock.Any()).Do(
		func(change ecs.TaskStateChange) {
			wg.Done()
		}).Times(3)

	handler.AddStateChangeEvent(taskEvent1, client)
	handler.AddStateChangeEvent(taskEvent2, client)
	handler.AddStateChangeEvent(taskEvent3, client)

	wg.Wait()

	// Wait for task events to be removed from the tasksToEvents map
	for {
		if getTasksToEventsLen(handler) == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
}

// getTasksToEventsLen returns the length of the tasksToEvents map. It is
// used only in the test code to ascertain that map has been cleaned up
func getTasksToEventsLen(handler *TaskHandler) int {
	handler.lock.RLock()
	defer handler.lock.RUnlock()

	return len(handler.tasksToEvents)
}

func containerEvent(arn string) statechange.Event {
	return api.ContainerStateChange{TaskArn: arn, ContainerName: "containerName", Status: apicontainerstatus.ContainerRunning, Container: &apicontainer.Container{}}
}

func managedAgentEvent(arn string) statechange.Event {
	return api.ManagedAgentStateChange{TaskArn: arn, Container: &apicontainer.Container{}, Name: "ExecAgent", Status: apicontainerstatus.ManagedAgentRunning}
}

func containerEventStopped(arn string) statechange.Event {
	return api.ContainerStateChange{TaskArn: arn, ContainerName: "containerName", Status: apicontainerstatus.ContainerStopped, Container: &apicontainer.Container{}}
}

func taskEvent(arn string) statechange.Event {
	return api.TaskStateChange{TaskARN: arn, Status: apitaskstatus.TaskRunning, Task: &apitask.Task{}}
}

func taskEventStopped(arn string) statechange.Event {
	return api.TaskStateChange{TaskARN: arn, Status: apitaskstatus.TaskStopped, Task: &apitask.Task{}}
}

func TestENISentStatusChange(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	client := mock_ecs.NewMockECSClient(ctrl)

	task := &apitask.Task{
		Arn: taskARN,
	}

	eniAttachment := &ni.ENIAttachment{
		AttachmentInfo: attachment.AttachmentInfo{
			TaskARN:          taskARN,
			AttachStatusSent: false,
			ExpiresAt:        time.Now().Add(time.Second),
		},
	}
	timeoutFunc := func() {
		eniAttachment.AttachStatusSent = true
	}
	assert.NoError(t, eniAttachment.StartTimer(timeoutFunc))

	sendableTaskEvent := newSendableTaskEvent(api.TaskStateChange{
		Attachment: eniAttachment,
		TaskARN:    taskARN,
		Status:     apitaskstatus.TaskStatusNone,
		Task:       task,
	})

	client.EXPECT().SubmitTaskStateChange(gomock.Any()).Return(nil)

	events := list.New()
	events.PushBack(sendableTaskEvent)
	ctx, cancel := context.WithCancel(context.Background())
	handler := NewTaskHandler(ctx, data.NewNoopClient(), dockerstate.NewTaskEngineState(), client)
	defer cancel()
	handler.submitTaskEvents(&taskSendableEvents{
		events: events,
	}, client, taskARN)

	assert.True(t, eniAttachment.AttachStatusSent)
}

func TestGetBatchedContainerEvents(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	state := mock_dockerstate.NewMockTaskEngineState(ctrl)

	handler := &TaskHandler{
		tasksToContainerStates: map[string][]api.ContainerStateChange{
			"t1": {},
			"t2": {},
		},
		state: state,
	}

	state.EXPECT().TaskByArn("t1").Return(&apitask.Task{Arn: "t1", KnownStatusUnsafe: apitaskstatus.TaskRunning}, true)
	state.EXPECT().TaskByArn("t2").Return(nil, false)

	events := handler.taskStateChangesToSend()
	assert.Len(t, events, 1)
	assert.Equal(t, "t1", events[0].TaskARN)
}

func TestGetBatchedContainerEventsStoppedTask(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	state := mock_dockerstate.NewMockTaskEngineState(ctrl)

	handler := &TaskHandler{
		tasksToContainerStates: map[string][]api.ContainerStateChange{
			"t1": {},
		},
		state: state,
	}

	state.EXPECT().TaskByArn("t1").Return(&apitask.Task{Arn: "t1", KnownStatusUnsafe: apitaskstatus.TaskStopped}, true)

	events := handler.taskStateChangesToSend()
	assert.Len(t, events, 0)
}

func TestSubmitTaskEventsWhenSubmittingTaskRunningAfterStopped(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	state := mock_dockerstate.NewMockTaskEngineState(ctrl)
	client := mock_ecs.NewMockECSClient(ctrl)

	handler := &TaskHandler{
		state:                  state,
		submitSemaphore:        utils.NewSemaphore(concurrentEventCalls),
		tasksToEvents:          make(map[string]*taskSendableEvents),
		tasksToContainerStates: make(map[string][]api.ContainerStateChange),
		client:                 client,
		dataClient:             data.NewNoopClient(),
	}

	taskEvents := &taskSendableEvents{events: list.New(),
		sending:   false,
		createdAt: time.Now(),
		taskARN:   taskARN,
	}

	backoff := mock_retry.NewMockBackoff(ctrl)
	ok, err := taskEvents.submitFirstEvent(handler, backoff)
	assert.True(t, ok)
	assert.NoError(t, err)

	task := &apitask.Task{}
	taskEvents.events.PushBack(newSendableTaskEvent(api.TaskStateChange{
		TaskARN: taskARN,
		Status:  apitaskstatus.TaskStopped,
		Task:    task,
	}))
	taskEvents.events.PushBack(newSendableTaskEvent(api.TaskStateChange{
		TaskARN: taskARN,
		Status:  apitaskstatus.TaskRunning,
		Task:    task,
	}))
	handler.tasksToEvents[taskARN] = taskEvents

	var wg sync.WaitGroup
	wg.Add(1)
	gomock.InOrder(
		client.EXPECT().SubmitTaskStateChange(gomock.Any()).Do(func(change ecs.TaskStateChange) {
			assert.Equal(t, apitaskstatus.TaskStopped, change.Status)
		}),
		backoff.EXPECT().Reset().Do(func() {
			wg.Done()
		}),
	)
	state.EXPECT().TaskByArn(gomock.Any()).AnyTimes().Return(task, true)
	ok, err = taskEvents.submitFirstEvent(handler, backoff)
	// We have an unsent event for the TaskRunning transition. Hence, send() returns false
	assert.False(t, ok)
	assert.NoError(t, err)
	wg.Wait()

	// The unsent transition is deleted from the task list. send() returns true as it
	// does not have any more events to process
	ok, err = taskEvents.submitFirstEvent(handler, backoff)
	assert.NoError(t, err)
	assert.True(t, ok)
}

func TestSubmitTaskEventsWhenSubmittingTaskStoppedAfterRunning(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	state := mock_dockerstate.NewMockTaskEngineState(ctrl)
	client := mock_ecs.NewMockECSClient(ctrl)

	handler := &TaskHandler{
		state:                  state,
		submitSemaphore:        utils.NewSemaphore(concurrentEventCalls),
		tasksToEvents:          make(map[string]*taskSendableEvents),
		tasksToContainerStates: make(map[string][]api.ContainerStateChange),
		client:                 client,
		dataClient:             data.NewNoopClient(),
	}

	taskEvents := &taskSendableEvents{events: list.New(),
		sending:   false,
		createdAt: time.Now(),
		taskARN:   taskARN,
	}

	backoff := mock_retry.NewMockBackoff(ctrl)
	ok, err := taskEvents.submitFirstEvent(handler, backoff)
	assert.True(t, ok)
	assert.NoError(t, err)

	task := &apitask.Task{}
	taskEvents.events.PushBack(newSendableTaskEvent(api.TaskStateChange{
		TaskARN: taskARN,
		Status:  apitaskstatus.TaskRunning,
		Task:    task,
	}))
	taskEvents.events.PushBack(newSendableTaskEvent(api.TaskStateChange{
		TaskARN: taskARN,
		Status:  apitaskstatus.TaskStopped,
		Task:    task,
	}))
	handler.tasksToEvents[taskARN] = taskEvents

	var wg sync.WaitGroup
	wg.Add(1)
	gomock.InOrder(
		client.EXPECT().SubmitTaskStateChange(gomock.Any()).Do(func(change ecs.TaskStateChange) {
			assert.Equal(t, apitaskstatus.TaskRunning, change.Status)
		}),
		backoff.EXPECT().Reset().Do(func() {
			wg.Done()
		}),
	)
	state.EXPECT().TaskByArn(gomock.Any()).AnyTimes().Return(task, true)
	// We have an unsent event for the TaskStopped transition. Hence, send() returns false
	ok, err = taskEvents.submitFirstEvent(handler, backoff)
	assert.False(t, ok)
	assert.NoError(t, err)
	wg.Wait()

	wg.Add(1)
	gomock.InOrder(
		client.EXPECT().SubmitTaskStateChange(gomock.Any()).Do(func(change ecs.TaskStateChange) {
			assert.Equal(t, apitaskstatus.TaskStopped, change.Status)
		}),
		backoff.EXPECT().Reset().Do(func() {
			wg.Done()
		}),
	)
	// The unsent transition is send and deleted from the task list. send() returns true as it
	// does not have any more events to process
	ok, err = taskEvents.submitFirstEvent(handler, backoff)
	assert.True(t, ok)
	assert.NoError(t, err)
	wg.Wait()
}

func TestSendContainerAndManagedAgentEvents(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	client := mock_ecs.NewMockECSClient(ctrl)

	ctx, cancel := context.WithCancel(context.Background())
	handler := NewTaskHandler(ctx, data.NewNoopClient(), dockerstate.NewTaskEngineState(), client)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)

	maEvent1 := managedAgentEvent(taskARN)
	cEevent1 := containerEvent(taskARN)
	taskEvent1 := taskEvent(taskARN)

	client.EXPECT().SubmitTaskStateChange(gomock.Any()).Do(func(change ecs.TaskStateChange) {
		assert.Equal(t, 1, len(change.ManagedAgents))
		assert.Equal(t, 1, len(change.Containers))
		assert.Equal(t, taskARN, change.TaskARN)
		wg.Done()
	})

	handler.AddStateChangeEvent(maEvent1, client)
	handler.AddStateChangeEvent(cEevent1, client)
	handler.AddStateChangeEvent(taskEvent1, client)

	wg.Wait()
	assert.Len(t, handler.tasksToManagedAgentStates, 0)
	assert.Len(t, handler.tasksToContainerStates, 0)
}

func TestSendManagedAgentEventsTaskDifferences(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	client := mock_ecs.NewMockECSClient(ctrl)
	dataClient := data.NewNoopClient()

	ctx, cancel := context.WithCancel(context.Background())
	handler := NewTaskHandler(ctx, dataClient, dockerstate.NewTaskEngineState(), client)
	defer cancel()

	taskARNA := "taskarnA"
	taskARNB := "taskarnB"

	var wg sync.WaitGroup
	wg.Add(2)

	var wgAddEvent sync.WaitGroup
	wgAddEvent.Add(1)

	// Test task event replacement doesn't happen
	taskEventA := taskEvent(taskARNA)
	maEventA1 := managedAgentEvent(taskARNA)

	maEventB1 := managedAgentEvent(taskARNB)
	taskEventB := taskEventStopped(taskARNB)

	client.EXPECT().SubmitTaskStateChange(gomock.Any()).Do(func(change ecs.TaskStateChange) {
		assert.Equal(t, taskARNA, change.TaskARN)
		wgAddEvent.Done()
		wg.Done()
	})

	client.EXPECT().SubmitTaskStateChange(gomock.Any()).Do(func(change ecs.TaskStateChange) {
		assert.Equal(t, taskARNB, change.TaskARN)
		wg.Done()
	})

	handler.AddStateChangeEvent(maEventB1, client)
	handler.AddStateChangeEvent(maEventA1, client)

	handler.AddStateChangeEvent(taskEventA, client)
	wgAddEvent.Wait()

	handler.AddStateChangeEvent(taskEventB, client)

	wg.Wait()
}

func TestGetBatchedManagedAgentEvents(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	state := mock_dockerstate.NewMockTaskEngineState(ctrl)

	handler := &TaskHandler{
		tasksToManagedAgentStates: map[string][]api.ManagedAgentStateChange{
			"t1": {},
			"t2": {},
		},
		state: state,
	}

	state.EXPECT().TaskByArn("t1").Return(&apitask.Task{Arn: "t1", KnownStatusUnsafe: apitaskstatus.TaskRunning}, true)
	state.EXPECT().TaskByArn("t2").Return(nil, false)

	events := handler.taskStateChangesToSend()
	assert.Len(t, events, 1)
	assert.Equal(t, "t1", events[0].TaskARN)
}

func TestGetBatchedManagedAgentEventsStoppedTask(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	state := mock_dockerstate.NewMockTaskEngineState(ctrl)

	handler := &TaskHandler{
		tasksToManagedAgentStates: map[string][]api.ManagedAgentStateChange{
			"t1": {},
		},
		state: state,
	}

	state.EXPECT().TaskByArn("t1").Return(&apitask.Task{Arn: "t1", KnownStatusUnsafe: apitaskstatus.TaskStopped}, true)

	events := handler.taskStateChangesToSend()
	assert.Len(t, events, 0)
}
