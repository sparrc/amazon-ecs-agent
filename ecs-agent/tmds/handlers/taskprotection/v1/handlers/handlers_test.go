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

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	mock_api "github.com/aws/amazon-ecs-agent/ecs-agent/api/ecs/mocks"
	apierrors "github.com/aws/amazon-ecs-agent/ecs-agent/api/errors"
	"github.com/aws/amazon-ecs-agent/ecs-agent/credentials"
	mock_credentials "github.com/aws/amazon-ecs-agent/ecs-agent/credentials/mocks"
	"github.com/aws/amazon-ecs-agent/ecs-agent/metrics"
	mock_metrics "github.com/aws/amazon-ecs-agent/ecs-agent/metrics/mocks"
	"github.com/aws/amazon-ecs-agent/ecs-agent/tmds/handlers/taskprotection/v1/types"
	v2 "github.com/aws/amazon-ecs-agent/ecs-agent/tmds/handlers/v2"
	"github.com/aws/amazon-ecs-agent/ecs-agent/tmds/handlers/v4/state"
	mock_state "github.com/aws/amazon-ecs-agent/ecs-agent/tmds/handlers/v4/state/mocks"
	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/golang/mock/gomock"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	cluster                         = "cluster"
	endpointId                      = "endpointId"
	ecsCallTimeout                  = 5 * time.Second
	taskARN                         = "taskARN"
	taskRoleCredsID                 = "taskRoleCredsID"
	updateTaskProtectionDecodeError = "UpdateTaskProtection: failed to decode request"
)

var (
	taskMetadataErrorResponse          = `{"error":{"Code":"ServerException","Message":"Failed to find a task for the request"}}`
	noCredentialsErrorResponse         = fmt.Sprintf(`{"error":{"Arn":"%s","Code":"AccessDeniedException","Message":"Invalid Request: no task IAM role credentials available for task"}}`, taskARN)
	requestFailureErrorResponse        = `{"requestID":"%s","error":{"Arn":"%s","Code":"AccessDeniedException","Message":"%s"}}`
	timeoutErrorResponse               = fmt.Sprintf(`{"error":{"Arn":"%s","Code":"RequestCanceled","Message":"Timed out calling ECS Task Protection API"}}`, taskARN)
	nonRequestAWSErrorResponse         = `{"error":{"Arn":"%s","Code":"InvalidParameterException","Message":"%s"}}`
	nonAWSErrorResponse                = `{"error":{"Arn":"%s","Code":"ServerException","Message":"%s"}}`
	ecsErrorResponse                   = fmt.Sprintf(`{"failure":{"Arn":"%s","Detail":null,"Reason":"ecs failure 1"}}`, taskARN)
	multipleECSErrorResponse           = fmt.Sprintf(`{"error":{"Arn":"%s","Code":"ServerException","Message":"Unexpected error occurred"}}`, taskARN)
	happyEnabledTaskProtectionResponse = fmt.Sprintf(`{"protection":{"ExpirationDate":null,"ProtectionEnabled":true,"TaskArn":"%s"}}`, taskARN)
	malformedRequestResponse           = `{"error":{"Code":"InvalidParameterException","Message":"%s"}}`
	missingTaskProtectionFieldResponse = fmt.Sprintf(`{"error":{"Arn":"%s","Code":"InvalidParameterException","Message":"Invalid request: does not contain 'ProtectionEnabled' field"}}`, taskARN)
)

// Tests the path for UpdateTaskProtection API
func TestTaskProtectionPath(t *testing.T) {
	assert.Equal(t, "/api/{endpointContainerIDMuxName:[^/]*}/task-protection/v1/state", TaskProtectionPath())
}

type TestCase struct {
	requestBody                 interface{} // Required for UpdateTaskProtection
	setAgentStateExpectations   func(agentState *mock_state.MockAgentState)
	setCredsManagerExpectations func(credsManager *mock_credentials.MockManager)
	setFactoryExpectations      func(ctrl *gomock.Controller, factory *MockTaskProtectionClientFactoryInterface)
	setMetricsExpectations      func(ctrl *gomock.Controller, metricsFactory *mock_metrics.MockEntryFactory)
	expectedStatusCode          int
	expectedResponseBody        types.TaskProtectionResponse
	expectedResonseBodyJSON     string
}

func testTaskProtectionRequest(t *testing.T, tc TestCase) {
	// Mocks
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	agentState := mock_state.NewMockAgentState(ctrl)
	credsManager := mock_credentials.NewMockManager(ctrl)
	factory := NewMockTaskProtectionClientFactoryInterface(ctrl)
	metricsFactory := mock_metrics.NewMockEntryFactory(ctrl)

	if tc.setAgentStateExpectations != nil {
		tc.setAgentStateExpectations(agentState)
	}
	if tc.setCredsManagerExpectations != nil {
		tc.setCredsManagerExpectations(credsManager)
	}
	if tc.setFactoryExpectations != nil {
		tc.setFactoryExpectations(ctrl, factory)
	}
	if tc.setMetricsExpectations != nil {
		tc.setMetricsExpectations(ctrl, metricsFactory)
	}

	// Setup the handlers
	router := mux.NewRouter()
	router.HandleFunc(
		TaskProtectionPath(),
		GetTaskProtectionHandler(agentState, credsManager, factory, cluster, metricsFactory, ecsCallTimeout),
	).Methods("GET")
	router.HandleFunc(
		TaskProtectionPath(),
		UpdateTaskProtectionHandler(agentState, credsManager, factory, cluster, metricsFactory, ecsCallTimeout),
	).Methods("PUT")

	// Create the request
	method := "GET"
	var requestBody io.Reader
	if tc.requestBody != nil {
		method = "PUT"
		reqBodyBytes, err := json.Marshal(tc.requestBody)
		require.NoError(t, err)
		requestBody = bytes.NewReader(reqBodyBytes)
	}
	req, err := http.NewRequest(method, fmt.Sprintf("/api/%s/task-protection/v1/state", endpointId),
		requestBody)
	require.NoError(t, err)

	// Send the request and record the response
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	// Parse the response body
	var actualResponseBody types.TaskProtectionResponse
	err = json.Unmarshal(recorder.Body.Bytes(), &actualResponseBody)
	require.NoError(t, err)
	assert.Equal(t, tc.expectedResonseBodyJSON, recorder.Body.String())

	// Assert status code and body
	assert.Equal(t, tc.expectedStatusCode, recorder.Code)
	assert.Equal(t, tc.expectedResponseBody, actualResponseBody)
}

func TestGetTaskProtection(t *testing.T) {
	// Initialize some data common to the test cases
	happyECSInput := ecs.GetTaskProtectionInput{
		Cluster: aws.String(cluster),
		Tasks:   []string{taskARN},
	}
	metricName := metrics.GetTaskProtectionMetricName

	// A helper function for setting expectations on mock ECS Client Factory
	factoryExpectations := func(
		input ecs.GetTaskProtectionInput,
		output *ecs.GetTaskProtectionOutput,
		err error,
	) func(*gomock.Controller, *MockTaskProtectionClientFactoryInterface) {
		return func(ctrl *gomock.Controller, factory *MockTaskProtectionClientFactoryInterface) {
			client := mock_api.NewMockECSTaskProtectionSDK(ctrl)
			client.EXPECT().GetTaskProtection(gomock.Any(), &input, gomock.Any()).Return(output, err)
			factory.EXPECT().NewTaskProtectionClient(taskRoleCreds()).Return(client, nil)
		}
	}

	// Test cases start here
	t.Run("task lookup failure", func(t *testing.T) {
		testTaskProtectionRequest(t, taskMetadataLookupFailureCase(metricName, nil))
	})
	t.Run("task metadata fetch failure", func(t *testing.T) {
		testTaskProtectionRequest(t, taskMetadataFetchErrorCase(
			state.NewErrorMetadataFetchFailure(""), metricName, nil))
	})
	t.Run("task metadata unknown error", func(t *testing.T) {
		testTaskProtectionRequest(t, taskMetadataFetchErrorCase(
			errors.New("unknown"), metricName, nil))
	})
	t.Run("task role creds not found", func(t *testing.T) {
		testTaskProtectionRequest(t, taskRoleCredsNotFoundCase(metricName, nil, noCredentialsErrorResponse))
	})
	t.Run("request failure", func(t *testing.T) {
		ecsRequestID := "reqID"
		ecsErrMessage := "ecs error"
		testTaskProtectionRequest(t, TestCase{
			setAgentStateExpectations:   happyStateExpectations,
			setCredsManagerExpectations: happyCredsManagerExpectations,
			setFactoryExpectations: factoryExpectations(happyECSInput, nil,
				&awshttp.ResponseError{
					ResponseError: &smithyhttp.ResponseError{
						Response: &smithyhttp.Response{
							Response: &http.Response{
								StatusCode: http.StatusBadRequest,
							},
						},
						Err: &ecstypes.AccessDeniedException{Message: &ecsErrMessage},
					},
					RequestID: ecsRequestID,
				},
			),
			setMetricsExpectations: metricsExpectations(metricName, 0),
			expectedStatusCode:     http.StatusBadRequest,
			expectedResponseBody: types.TaskProtectionResponse{
				RequestID: &ecsRequestID,
				Error: &types.ErrorResponse{
					Arn:     taskARN,
					Code:    apierrors.ErrCodeAccessDeniedException,
					Message: ecsErrMessage,
				},
			},
			expectedResonseBodyJSON: fmt.Sprintf(requestFailureErrorResponse, ecsRequestID, taskARN, ecsErrMessage),
		})
	})
	t.Run("agent timeout", func(t *testing.T) {
		testTaskProtectionRequest(t, TestCase{
			setAgentStateExpectations:   happyStateExpectations,
			setCredsManagerExpectations: happyCredsManagerExpectations,
			setFactoryExpectations: factoryExpectations(happyECSInput, nil,
				&aws.RequestCanceledError{Err: context.Canceled},
			),
			setMetricsExpectations: metricsExpectations(metricName, 0),
			expectedStatusCode:     http.StatusGatewayTimeout,
			expectedResponseBody: types.TaskProtectionResponse{
				Error: &types.ErrorResponse{
					Arn:     taskARN,
					Code:    requestCanceled,
					Message: "Timed out calling ECS Task Protection API",
				},
			},
			expectedResonseBodyJSON: timeoutErrorResponse,
		})
	})
	t.Run("non-request-failure aws error", func(t *testing.T) {
		ecsErrMessage := "ecs error"
		testTaskProtectionRequest(t, TestCase{
			setAgentStateExpectations:   happyStateExpectations,
			setCredsManagerExpectations: happyCredsManagerExpectations,
			setFactoryExpectations: factoryExpectations(happyECSInput, nil,
				&ecstypes.InvalidParameterException{Message: &ecsErrMessage}),
			setMetricsExpectations: metricsExpectations(metricName, 0),
			expectedStatusCode:     http.StatusInternalServerError,
			expectedResponseBody: types.TaskProtectionResponse{
				Error: &types.ErrorResponse{
					Arn:     taskARN,
					Code:    apierrors.ErrCodeInvalidParameterException,
					Message: ecsErrMessage,
				},
			},
			expectedResonseBodyJSON: fmt.Sprintf(nonRequestAWSErrorResponse, taskARN, ecsErrMessage),
		})
	})
	t.Run("non-aws error", func(t *testing.T) {
		err := errors.New("some error")
		testTaskProtectionRequest(t, TestCase{
			setAgentStateExpectations:   happyStateExpectations,
			setCredsManagerExpectations: happyCredsManagerExpectations,
			setFactoryExpectations:      factoryExpectations(happyECSInput, nil, err),
			setMetricsExpectations:      metricsExpectations(metricName, 0),
			expectedStatusCode:          http.StatusInternalServerError,
			expectedResponseBody: types.TaskProtectionResponse{
				Error: &types.ErrorResponse{
					Arn: taskARN, Code: apierrors.ErrCodeServerException, Message: err.Error(),
				},
			},
			expectedResonseBodyJSON: fmt.Sprintf(nonAWSErrorResponse, taskARN, err.Error()),
		})
	})
	t.Run("ecs failure", func(t *testing.T) {
		ecsFailure := makeECSFailure("ecs failure")
		testTaskProtectionRequest(t, TestCase{
			setAgentStateExpectations:   happyStateExpectations,
			setCredsManagerExpectations: happyCredsManagerExpectations,
			setFactoryExpectations: factoryExpectations(happyECSInput, &ecs.GetTaskProtectionOutput{
				Failures: []ecstypes.Failure{ecsFailure},
			}, nil),
			setMetricsExpectations: metricsExpectations(metricName, 0),
			expectedStatusCode:     http.StatusOK,
			expectedResponseBody: types.TaskProtectionResponse{
				Failure: &ecsFailure,
			},
			expectedResonseBodyJSON: ecsErrorResponse,
		})
	})
	t.Run("more than one ecs failure", func(t *testing.T) {
		testTaskProtectionRequest(t, TestCase{
			setAgentStateExpectations:   happyStateExpectations,
			setCredsManagerExpectations: happyCredsManagerExpectations,
			setFactoryExpectations: factoryExpectations(happyECSInput, &ecs.GetTaskProtectionOutput{
				Failures: []ecstypes.Failure{makeECSFailure("1"), makeECSFailure("2")},
			}, nil),
			setMetricsExpectations: metricsExpectations(metricName, 0),
			expectedStatusCode:     http.StatusInternalServerError,
			expectedResponseBody: types.TaskProtectionResponse{
				Error: &types.ErrorResponse{
					Arn:     taskARN,
					Code:    apierrors.ErrCodeServerException,
					Message: "Unexpected error occurred",
				},
			},
			expectedResonseBodyJSON: multipleECSErrorResponse,
		})
	})
	t.Run("happy case", func(t *testing.T) {
		protectedTask := ecsProtectedTask()
		testTaskProtectionRequest(t, TestCase{
			setAgentStateExpectations:   happyStateExpectations,
			setCredsManagerExpectations: happyCredsManagerExpectations,
			setFactoryExpectations: factoryExpectations(happyECSInput, &ecs.GetTaskProtectionOutput{
				ProtectedTasks: []ecstypes.ProtectedTask{protectedTask},
			}, nil),
			setMetricsExpectations:  metricsExpectations(metricName, 1),
			expectedStatusCode:      http.StatusOK,
			expectedResponseBody:    types.TaskProtectionResponse{Protection: &protectedTask},
			expectedResonseBodyJSON: happyEnabledTaskProtectionResponse,
		})
	})
}

func TestUpdateTaskProtection(t *testing.T) {
	// Initialize some data common to the test cases
	metricName := metrics.UpdateTaskProtectionMetricName
	expiresInMinutes := 5
	protectionEnabled := true
	happyRequestBody := &TaskProtectionRequest{
		ProtectionEnabled: aws.Bool(protectionEnabled), ExpiresInMinutes: aws.Int64(int64(expiresInMinutes)),
	}
	happyECSInput := ecs.UpdateTaskProtectionInput{
		Cluster:           aws.String(cluster),
		Tasks:             []string{taskARN},
		ExpiresInMinutes:  aws.Int32(int32(expiresInMinutes)),
		ProtectionEnabled: protectionEnabled,
	}

	// A helper function for setting expectations on mock ECS Client Factory
	factoryExpectations := func(
		input ecs.UpdateTaskProtectionInput,
		output *ecs.UpdateTaskProtectionOutput,
		err error,
	) func(*gomock.Controller, *MockTaskProtectionClientFactoryInterface) {
		return func(ctrl *gomock.Controller, factory *MockTaskProtectionClientFactoryInterface) {
			client := mock_api.NewMockECSTaskProtectionSDK(ctrl)
			client.EXPECT().UpdateTaskProtection(gomock.Any(), &input, gomock.Any()).Return(output, err)
			factory.EXPECT().NewTaskProtectionClient(taskRoleCreds()).Return(client, nil)
		}
	}

	// Test cases start here
	t.Run("task lookup failure", func(t *testing.T) {
		testTaskProtectionRequest(t, taskMetadataLookupFailureCase(metricName, happyRequestBody))
	})
	t.Run("task metadata fetch failure", func(t *testing.T) {
		testTaskProtectionRequest(t, taskMetadataFetchErrorCase(
			state.NewErrorMetadataFetchFailure(""), metricName, happyRequestBody))
	})
	t.Run("task metadata unknown error", func(t *testing.T) {
		testTaskProtectionRequest(t, taskMetadataFetchErrorCase(
			errors.New("unknown"), metricName, happyRequestBody))
	})
	t.Run("unknown field in request", func(t *testing.T) {
		testTaskProtectionRequest(t, TestCase{
			requestBody: map[string]interface{}{
				"ProtectionEnabled": true,
				"ExpiresInMinutes":  5,
				"Unknown":           2,
			},
			setMetricsExpectations: nil, // no metrics interaction expected
			expectedStatusCode:     http.StatusBadRequest,
			expectedResponseBody: types.TaskProtectionResponse{
				Error: &types.ErrorResponse{
					Code:    apierrors.ErrCodeInvalidParameterException,
					Message: updateTaskProtectionDecodeError,
				},
			},
			expectedResonseBodyJSON: fmt.Sprintf(malformedRequestResponse, updateTaskProtectionDecodeError),
		})
	})
	t.Run("invalid type in the request", func(t *testing.T) {
		testTaskProtectionRequest(t, TestCase{
			requestBody:            map[string]interface{}{"ProtectionEnabled": "bad"},
			setMetricsExpectations: nil, // no metrics interaction expected
			expectedStatusCode:     http.StatusBadRequest,
			expectedResponseBody: types.TaskProtectionResponse{
				Error: &types.ErrorResponse{
					Code:    apierrors.ErrCodeInvalidParameterException,
					Message: updateTaskProtectionDecodeError,
				},
			},
			expectedResonseBodyJSON: fmt.Sprintf(malformedRequestResponse, updateTaskProtectionDecodeError),
		})
	})
	t.Run("ProtectionEnabled field not found on the request", func(t *testing.T) {
		testTaskProtectionRequest(t, TestCase{
			requestBody:               &TaskProtectionRequest{ExpiresInMinutes: aws.Int64(int64(expiresInMinutes))},
			setAgentStateExpectations: happyStateExpectations,
			setMetricsExpectations: func(ctrl *gomock.Controller, metricsFactory *mock_metrics.MockEntryFactory) {
				// expecting entry creation but no publish
				entry := mock_metrics.NewMockEntry(ctrl)
				metricsFactory.EXPECT().New(metricName).Return(entry)
			},
			expectedStatusCode: http.StatusBadRequest,
			expectedResponseBody: types.TaskProtectionResponse{
				Error: &types.ErrorResponse{
					Arn:     taskARN,
					Code:    apierrors.ErrCodeInvalidParameterException,
					Message: "Invalid request: does not contain 'ProtectionEnabled' field",
				},
			},
			expectedResonseBodyJSON: missingTaskProtectionFieldResponse,
		})
	})
	t.Run("task role creds not found", func(t *testing.T) {
		testTaskProtectionRequest(t, taskRoleCredsNotFoundCase(metricName, happyRequestBody, noCredentialsErrorResponse))
	})
	t.Run("request failure", func(t *testing.T) {
		ecsRequestID := "reqID"
		ecsErrMessage := "ecs error"
		testTaskProtectionRequest(t, TestCase{
			requestBody:                 happyRequestBody,
			setAgentStateExpectations:   happyStateExpectations,
			setCredsManagerExpectations: happyCredsManagerExpectations,
			setFactoryExpectations: factoryExpectations(happyECSInput, nil,
				&awshttp.ResponseError{
					ResponseError: &smithyhttp.ResponseError{
						Response: &smithyhttp.Response{
							Response: &http.Response{
								StatusCode: http.StatusBadRequest,
							},
						},
						Err: &ecstypes.AccessDeniedException{Message: &ecsErrMessage},
					},
					RequestID: ecsRequestID,
				},
			),
			setMetricsExpectations: metricsExpectations(metricName, 0),
			expectedStatusCode:     http.StatusBadRequest,
			expectedResponseBody: types.TaskProtectionResponse{
				RequestID: &ecsRequestID,
				Error: &types.ErrorResponse{
					Arn:     taskARN,
					Code:    apierrors.ErrCodeAccessDeniedException,
					Message: ecsErrMessage,
				},
			},
			expectedResonseBodyJSON: fmt.Sprintf(requestFailureErrorResponse, ecsRequestID, taskARN, ecsErrMessage),
		})
	})
	t.Run("agent timeout", func(t *testing.T) {
		testTaskProtectionRequest(t, TestCase{
			requestBody:                 happyRequestBody,
			setAgentStateExpectations:   happyStateExpectations,
			setCredsManagerExpectations: happyCredsManagerExpectations,
			setFactoryExpectations: factoryExpectations(happyECSInput, nil,
				&aws.RequestCanceledError{Err: context.Canceled},
			),
			setMetricsExpectations: metricsExpectations(metricName, 0),
			expectedStatusCode:     http.StatusGatewayTimeout,
			expectedResponseBody: types.TaskProtectionResponse{
				Error: &types.ErrorResponse{
					Arn:     taskARN,
					Code:    requestCanceled,
					Message: "Timed out calling ECS Task Protection API",
				},
			},
			expectedResonseBodyJSON: timeoutErrorResponse,
		})
	})
	t.Run("non-request-failure aws error", func(t *testing.T) {
		ecsErrMessage := "ecs error"
		testTaskProtectionRequest(t, TestCase{
			requestBody:                 happyRequestBody,
			setAgentStateExpectations:   happyStateExpectations,
			setCredsManagerExpectations: happyCredsManagerExpectations,
			setFactoryExpectations: factoryExpectations(happyECSInput, nil,
				&ecstypes.InvalidParameterException{Message: &ecsErrMessage}),
			setMetricsExpectations: metricsExpectations(metricName, 0),
			expectedStatusCode:     http.StatusInternalServerError,
			expectedResponseBody: types.TaskProtectionResponse{
				Error: &types.ErrorResponse{
					Arn:     taskARN,
					Code:    apierrors.ErrCodeInvalidParameterException,
					Message: ecsErrMessage,
				},
			},
			expectedResonseBodyJSON: fmt.Sprintf(nonRequestAWSErrorResponse, taskARN, ecsErrMessage),
		})
	})
	t.Run("non-aws error", func(t *testing.T) {
		err := errors.New("some error")
		testTaskProtectionRequest(t, TestCase{
			requestBody:                 happyRequestBody,
			setAgentStateExpectations:   happyStateExpectations,
			setCredsManagerExpectations: happyCredsManagerExpectations,
			setFactoryExpectations:      factoryExpectations(happyECSInput, nil, err),
			setMetricsExpectations:      metricsExpectations(metricName, 0),
			expectedStatusCode:          http.StatusInternalServerError,
			expectedResponseBody: types.TaskProtectionResponse{
				Error: &types.ErrorResponse{
					Arn: taskARN, Code: apierrors.ErrCodeServerException, Message: err.Error(),
				},
			},
			expectedResonseBodyJSON: fmt.Sprintf(nonAWSErrorResponse, taskARN, err.Error()),
		})
	})
	t.Run("ecs failure", func(t *testing.T) {
		ecsFailure := makeECSFailure("ecs failure")
		testTaskProtectionRequest(t, TestCase{
			requestBody:                 happyRequestBody,
			setAgentStateExpectations:   happyStateExpectations,
			setCredsManagerExpectations: happyCredsManagerExpectations,
			setFactoryExpectations: factoryExpectations(happyECSInput, &ecs.UpdateTaskProtectionOutput{
				Failures: []ecstypes.Failure{ecsFailure},
			}, nil),
			setMetricsExpectations: metricsExpectations(metricName, 0),
			expectedStatusCode:     http.StatusOK,
			expectedResponseBody: types.TaskProtectionResponse{
				Failure: &ecsFailure,
			},
			expectedResonseBodyJSON: ecsErrorResponse,
		})
	})
	t.Run("more than one ecs failure", func(t *testing.T) {
		testTaskProtectionRequest(t, TestCase{
			requestBody:                 happyRequestBody,
			setAgentStateExpectations:   happyStateExpectations,
			setCredsManagerExpectations: happyCredsManagerExpectations,
			setFactoryExpectations: factoryExpectations(happyECSInput, &ecs.UpdateTaskProtectionOutput{
				Failures: []ecstypes.Failure{makeECSFailure("1"), makeECSFailure("2")},
			}, nil),
			setMetricsExpectations: metricsExpectations(metricName, 0),
			expectedStatusCode:     http.StatusInternalServerError,
			expectedResponseBody: types.TaskProtectionResponse{
				Error: &types.ErrorResponse{
					Arn:     taskARN,
					Code:    apierrors.ErrCodeServerException,
					Message: "Unexpected error occurred",
				},
			},
			expectedResonseBodyJSON: multipleECSErrorResponse,
		})
	})
	t.Run("happy case", func(t *testing.T) {
		protectedTask := ecsProtectedTask()
		testTaskProtectionRequest(t, TestCase{
			requestBody:                 happyRequestBody,
			setAgentStateExpectations:   happyStateExpectations,
			setCredsManagerExpectations: happyCredsManagerExpectations,
			setFactoryExpectations: factoryExpectations(happyECSInput, &ecs.UpdateTaskProtectionOutput{
				ProtectedTasks: []ecstypes.ProtectedTask{protectedTask},
			}, nil),
			setMetricsExpectations:  metricsExpectations(metricName, 1),
			expectedStatusCode:      http.StatusOK,
			expectedResponseBody:    types.TaskProtectionResponse{Protection: &protectedTask},
			expectedResonseBodyJSON: happyEnabledTaskProtectionResponse,
		})
	})
}

// Returns an ECS Failure with the given reason. Uses standard Task ARN.
func makeECSFailure(reason string) ecstypes.Failure {
	return ecstypes.Failure{
		Arn:    aws.String(taskARN),
		Reason: aws.String("ecs failure 1"),
	}
}

// Returns a standard ECS Protected Task for testing.
func ecsProtectedTask() ecstypes.ProtectedTask {
	return ecstypes.ProtectedTask{
		ProtectionEnabled: true,
		TaskArn:           aws.String(taskARN),
	}
}

// Returns a function that sets expectations on mock metrics factory.
// The expectation is for one entry to be created with the provided name and count values.
func metricsExpectations(
	name string,
	count int,
) func(*gomock.Controller, *mock_metrics.MockEntryFactory) {
	return func(ctrl *gomock.Controller, metricsFactory *mock_metrics.MockEntryFactory) {
		entry := mock_metrics.NewMockEntry(ctrl)
		gomock.InOrder(
			metricsFactory.EXPECT().New(name).Return(entry),
			entry.EXPECT().WithCount(count).Return(entry),
			entry.EXPECT().Done(nil),
		)
	}
}

// Function for setting happy case expectations on credentials manager.
// The expectation is for GetTaskCredentials method to be called with standard
// task role credentials ID returning standard task role credentials.
func happyCredsManagerExpectations(credsManager *mock_credentials.MockManager) {
	credsManager.EXPECT().GetTaskCredentials(taskRoleCredsID).Return(taskRoleCreds(), true)
}

// Returns a test case for Task Metadata fetch failure case.
func taskMetadataFetchErrorCase(err error, metricName string, reqBody interface{}) TestCase {
	return TestCase{
		setAgentStateExpectations: func(agentState *mock_state.MockAgentState) {
			agentState.EXPECT().GetTaskMetadata(endpointId).Return(state.TaskResponse{}, err)
		},
		setMetricsExpectations: metricsExpectations(metricName, 0),
		requestBody:            reqBody,
		expectedStatusCode:     http.StatusInternalServerError,
		expectedResponseBody: types.TaskProtectionResponse{
			Error: &types.ErrorResponse{
				Code:    apierrors.ErrCodeServerException,
				Message: "Failed to find a task for the request",
			},
		},
		expectedResonseBodyJSON: taskMetadataErrorResponse,
	}
}

// Returns a test case for Task Metadata Lookup failure case.
func taskMetadataLookupFailureCase(metricName string, reqBody interface{}) TestCase {
	err := state.NewErrorLookupFailure("external reason")
	return TestCase{
		setAgentStateExpectations: func(agentState *mock_state.MockAgentState) {
			agentState.EXPECT().GetTaskMetadata(endpointId).Return(state.TaskResponse{}, err)
		},
		setMetricsExpectations: func(ctrl *gomock.Controller, metricsFactory *mock_metrics.MockEntryFactory) {
			entry := mock_metrics.NewMockEntry(ctrl)
			metricsFactory.EXPECT().New(metricName).Return(entry)
		},
		requestBody:        reqBody,
		expectedStatusCode: http.StatusNotFound,
		expectedResponseBody: types.TaskProtectionResponse{
			Error: &types.ErrorResponse{
				Code:    apierrors.ErrCodeResourceNotFoundException,
				Message: "Failed to find a task for the request",
			},
		},
		expectedResonseBodyJSON: `{"error":{"Code":"ResourceNotFoundException","Message":"Failed to find a task for the request"}}`,
	}
}

// Creates a test case for Task Role credentials not found case.
func taskRoleCredsNotFoundCase(metricName string, reqBody interface{}, expectedResponseJSON string) TestCase {
	return TestCase{
		setAgentStateExpectations: happyStateExpectations,
		setCredsManagerExpectations: func(credsManager *mock_credentials.MockManager) {
			credsManager.EXPECT().GetTaskCredentials(taskRoleCredsID).
				Return(credentials.TaskIAMRoleCredentials{}, false)
		},
		setMetricsExpectations: metricsExpectations(metricName, 0),
		requestBody:            reqBody,
		expectedStatusCode:     http.StatusForbidden,
		expectedResponseBody: types.TaskProtectionResponse{
			Error: &types.ErrorResponse{
				Arn:     taskARN,
				Code:    apierrors.ErrCodeAccessDeniedException,
				Message: "Invalid Request: no task IAM role credentials available for task",
			},
		},
		expectedResonseBodyJSON: expectedResponseJSON,
	}
}

// Function for setting expectations on mock AgentState.
// The expectation is for GetTaskMetadata to be called with the test endpointID
// returning a standard test Task Metadata.
func happyStateExpectations(agentState *mock_state.MockAgentState) {
	agentState.EXPECT().GetTaskMetadata(endpointId).Return(state.TaskResponse{
		TaskResponse:  &v2.TaskResponse{TaskARN: taskARN},
		CredentialsID: taskRoleCredsID,
	}, nil)
}

// Returns standard Task Role credentials for testing.
func taskRoleCreds() credentials.TaskIAMRoleCredentials {
	return credentials.TaskIAMRoleCredentials{
		ARN: "taskRoleCredsARN",
		IAMRoleCredentials: credentials.IAMRoleCredentials{
			RoleArn:         "roleARN",
			AccessKeyID:     "accessKeyID",
			SecretAccessKey: "secretAccessKey",
		},
	}
}
