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

package utils

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"testing"
	"time"

	apierrors "github.com/aws/amazon-ecs-agent/ecs-agent/api/errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	serviceId              = "ec2imds"
	getMetadataOperationId = "GetMetadata"
)

func TestDefaultIfBlank(t *testing.T) {
	const defaultValue = "a boring default"
	const specifiedValue = "new value"
	result := DefaultIfBlank(specifiedValue, defaultValue)
	assert.Equal(t, specifiedValue, result)

	result = DefaultIfBlank("", defaultValue)
	assert.Equal(t, defaultValue, result)
}

func TestSlicesDeepEqual(t *testing.T) {
	testCases := []struct {
		left     []string
		right    []string
		expected bool
		name     string
	}{
		{[]string{}, []string{}, true, "Two empty slices"},
		{[]string{"cat"}, []string{}, false, "One empty slice"},
		{[]string{}, []string{"cat"}, false, "Another empty slice"},
		{[]string{"cat"}, []string{"cat"}, true, "Two slices with one element each"},
		{[]string{"cat", "dog", "cat"}, []string{"dog", "cat", "cat"}, true, "Two slices with multiple elements"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, SlicesDeepEqual(tc.left, tc.right))
		})
	}
}

func TestRemove(t *testing.T) {
	testSlice := []string{"cat", "dog", "cat"}
	removeElementAtIndex := 0

	expectedValue := []string{"dog", "cat"}
	actualValue := Remove(testSlice, removeElementAtIndex)

	assert.Equal(t, expectedValue, actualValue)
}

func TestParseBool(t *testing.T) {
	truthyStrings := []string{"true", "1", "t", "true\r", "true ", "true \r"}
	falsyStrings := []string{"false", "0", "f", "false\r", "false ", "false \r"}
	neitherStrings := []string{"apple", " ", "\r", "orange", "maybe"}

	for _, str := range truthyStrings {
		t.Run("truthy", func(t *testing.T) {
			assert.True(t, ParseBool(str, false), "Truthy string should be truthy")
			assert.True(t, ParseBool(str, true), "Truthy string should be truthy (regardless of default)")
		})
	}

	for _, str := range falsyStrings {
		t.Run("falsy", func(t *testing.T) {
			assert.False(t, ParseBool(str, false), "Falsy string should be falsy")
			assert.False(t, ParseBool(str, true), "Falsy string should be falsy (regardless of default)")
		})
	}

	for _, str := range neitherStrings {
		t.Run("defaults", func(t *testing.T) {
			assert.False(t, ParseBool(str, false), "Should default to false")
			assert.True(t, ParseBool(str, true), "Should default to true")
		})
	}
}

func TestIsAWSErrorCodeEqual(t *testing.T) {
	testcases := []struct {
		name string
		err  error
		res  bool
	}{
		{
			name: "Happy Path SDKv2",
			err:  &smithy.GenericAPIError{Code: apierrors.ErrCodeInvalidParameterException},
			res:  true,
		},
		{
			name: "Wrong Error Code SDKv2",
			err:  &smithy.GenericAPIError{Code: "errCode"},
		},
		{
			name: "Wrong Error Type",
			err:  errors.New("err"),
			res:  false,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.res, IsAWSErrorCodeEqual(tc.err, apierrors.ErrCodeInvalidParameterException))
		})
	}
}

func TestGetResponseErrorStatusCode(t *testing.T) {
	testcases := []struct {
		name string
		err  error
		res  int
	}{
		{
			name: "TestGetResponseErrorStatusCodeSuccess",
			err: &smithy.OperationError{
				ServiceID:     serviceId,
				OperationName: getMetadataOperationId,
				Err: &smithyhttp.ResponseError{
					Response: &smithyhttp.Response{
						Response: &http.Response{
							StatusCode: 400,
						},
					},
				},
			},
			res: 400,
		},
		{
			name: "TestGetResponseErrorStatusCodeCodeWrongErrType",
			err:  errors.New("err"),
			res:  0,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.res, GetResponseErrorStatusCode(tc.err))
		})
	}
}

func TestMapToTags(t *testing.T) {
	tagKey1 := "tagKey1"
	tagKey2 := "tagKey2"
	tagValue1 := "tagValue1"
	tagValue2 := "tagValue2"
	tagsMap := map[string]string{
		tagKey1: tagValue1,
		tagKey2: tagValue2,
	}
	tags := MapToTags(tagsMap)
	assert.Equal(t, 2, len(tags))
	sort.Slice(tags, func(i, j int) bool {
		return aws.ToString(tags[i].Key) < aws.ToString(tags[j].Key)
	})

	assert.Equal(t, aws.ToString(tags[0].Key), tagKey1)
	assert.Equal(t, aws.ToString(tags[0].Value), tagValue1)
	assert.Equal(t, aws.ToString(tags[1].Key), tagKey2)
	assert.Equal(t, aws.ToString(tags[1].Value), tagValue2)
}

func TestNilMapToTags(t *testing.T) {
	assert.Zero(t, len(MapToTags(nil)))
}

func TestGetTaskID(t *testing.T) {
	taskARN := "arn:aws:ecs:us-west-2:1234567890:task/test-cluster/abc"
	id, err := GetTaskID(taskARN)
	require.NoError(t, err)
	assert.Equal(t, "abc", id)

	_, err = GetTaskID("invalid")
	assert.Error(t, err)
}

func TestGetAttachmentId(t *testing.T) {
	attachmentArn := "arn:aws:ecs:us-west-2:1234567890:attachment/abc"
	id, err := GetAttachmentId(attachmentArn)
	require.NoError(t, err)
	assert.Equal(t, "abc", id)

	_, err = GetAttachmentId("invalid")
	assert.Error(t, err)
}

func TestFileExists(t *testing.T) {
	t.Run("file is found", func(t *testing.T) {
		file, err := os.CreateTemp("", "file_exists_test")
		res, err := FileExists(file.Name())
		assert.NoError(t, err)
		assert.True(t, res)
	})
	t.Run("file is not found", func(t *testing.T) {
		res, err := FileExists(fmt.Sprintf("test_file_exists_%d", time.Now().Unix()))
		assert.NoError(t, err)
		assert.False(t, res)
	})
}
