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

package dockerauth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"

	apicontainer "github.com/aws/amazon-ecs-agent/agent/api/container"
	mock_ecr "github.com/aws/amazon-ecs-agent/agent/ecr/mocks"
	"github.com/aws/amazon-ecs-agent/ecs-agent/async"
	mock_async "github.com/aws/amazon-ecs-agent/ecs-agent/async/mocks"
	"github.com/aws/amazon-ecs-agent/ecs-agent/credentials"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/docker/docker/api/types/registry"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testToken         = "testToken"
	testProxyEndpoint = "testProxyEndpoint"
	tokenCacheSize    = 100
	tokenCacheTTL     = 12 * time.Hour
)

func TestNewAuthProviderECRAuth(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	factory := mock_ecr.NewMockECRFactory(ctrl)

	provider := NewECRAuthProvider(factory, async.NewLRUCache(tokenCacheSize, tokenCacheTTL))
	_, ok := provider.(*ecrAuthProvider)
	assert.True(t, ok, "Should have returned ecrAuthProvider")
}

func TestGetAuthConfigSuccess(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	client := mock_ecr.NewMockECRClient(ctrl)
	factory := mock_ecr.NewMockECRFactory(ctrl)

	authData := &apicontainer.ECRAuthData{
		Region:           "us-west-2",
		RegistryID:       "0123456789012",
		EndpointOverride: "my.endpoint",
	}
	proxyEndpoint := "proxy"
	username := "username"
	password := "password"

	registryAuthData := &apicontainer.RegistryAuthenticationData{
		ECRAuthData: authData,
	}

	provider := ecrAuthProvider{
		factory:    factory,
		tokenCache: async.NewLRUCache(tokenCacheSize, tokenCacheTTL),
	}

	factory.EXPECT().GetClient(authData).Return(client, nil)
	client.EXPECT().GetAuthorizationToken(authData.RegistryID).Return(&types.AuthorizationData{
		ProxyEndpoint:      aws.String(proxyEndpointScheme + proxyEndpoint),
		AuthorizationToken: aws.String(base64.StdEncoding.EncodeToString([]byte(username + ":" + password))),
	}, nil)

	authconfig, err := provider.GetAuthconfig(proxyEndpoint+"/myimage", registryAuthData)
	require.NoError(t, err, "Unexpected error in getting auth config from ecr")

	assert.Equal(t, username, authconfig.Username, "Expected username to be %s, but was %s", username, authconfig.Username)
	assert.Equal(t, password, authconfig.Password, "Expected password to be %s, but was %s", password, authconfig.Password)
}

func TestGetAuthConfigNoMatchEndpointToImageUri(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	factory := mock_ecr.NewMockECRFactory(ctrl)
	client := mock_ecr.NewMockECRClient(ctrl)

	authData := &apicontainer.ECRAuthData{
		Region:           "us-west-2",
		RegistryID:       "0123456789012",
		EndpointOverride: "my.endpoint",
	}
	proxyEndpoint := "proxy"
	username := "username"
	password := "password"

	provider := ecrAuthProvider{
		factory:    factory,
		tokenCache: async.NewLRUCache(tokenCacheSize, tokenCacheTTL),
	}

	registryAuthData := &apicontainer.RegistryAuthenticationData{
		ECRAuthData: authData,
	}

	factory.EXPECT().GetClient(authData).Return(client, nil)
	client.EXPECT().GetAuthorizationToken(authData.RegistryID).Return(&types.AuthorizationData{
		ProxyEndpoint:      aws.String(proxyEndpointScheme + "notproxy"),
		AuthorizationToken: aws.String(base64.StdEncoding.EncodeToString([]byte(username + ":" + password))),
	}, nil)

	authconfig, err := provider.GetAuthconfig(proxyEndpoint+"/myimage", registryAuthData)
	assert.NoError(t, err)
	assert.Equal(t, username, authconfig.Username, "Expected username to be %s, but was %s", username, authconfig.Username)
	assert.Equal(t, password, authconfig.Password, "Expected password to be %s, but was %s", password, authconfig.Password)
}

func TestGetAuthConfigNilAuthorizationToken(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	factory := mock_ecr.NewMockECRFactory(ctrl)
	client := mock_ecr.NewMockECRClient(ctrl)

	authData := &apicontainer.ECRAuthData{
		Region:           "us-west-2",
		RegistryID:       "0123456789012",
		EndpointOverride: "my.endpoint",
	}
	proxyEndpoint := "proxy"

	provider := ecrAuthProvider{
		factory:    factory,
		tokenCache: async.NewLRUCache(tokenCacheSize, tokenCacheTTL),
	}

	registryAuthData := &apicontainer.RegistryAuthenticationData{
		ECRAuthData: authData,
	}

	factory.EXPECT().GetClient(authData).Return(client, nil)
	client.EXPECT().GetAuthorizationToken(authData.RegistryID).Return(&types.AuthorizationData{
		ProxyEndpoint:      aws.String(proxyEndpointScheme + "notproxy"),
		AuthorizationToken: nil,
	}, nil)

	authconfig, err := provider.GetAuthconfig(proxyEndpoint+"/myimage", registryAuthData)
	require.Error(t, err, "Expected error to be present, but was nil", err)
	assert.Equal(t, registry.AuthConfig{}, authconfig, "Expected Authconfig to be empty, but was %v", authconfig)
}

func TestGetAuthConfigBadBase64(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	factory := mock_ecr.NewMockECRFactory(ctrl)
	client := mock_ecr.NewMockECRClient(ctrl)

	authData := &apicontainer.ECRAuthData{
		Region:           "us-west-2",
		RegistryID:       "0123456789012",
		EndpointOverride: "my.endpoint",
	}
	proxyEndpoint := "proxy"
	username := "username"
	password := "password"

	provider := ecrAuthProvider{
		factory:    factory,
		tokenCache: async.NewLRUCache(tokenCacheSize, tokenCacheTTL),
	}

	registryAuthData := &apicontainer.RegistryAuthenticationData{
		ECRAuthData: authData,
	}

	factory.EXPECT().GetClient(authData).Return(client, nil)
	client.EXPECT().GetAuthorizationToken(authData.RegistryID).Return(&types.AuthorizationData{
		ProxyEndpoint:      aws.String(proxyEndpointScheme + "notproxy"),
		AuthorizationToken: aws.String((username + ":" + password)),
	}, nil)

	authconfig, err := provider.GetAuthconfig(proxyEndpoint+"/myimage", registryAuthData)
	require.Error(t, err, "Expected error to be present, but was nil", err)
	assert.Equal(t, registry.AuthConfig{}, authconfig, "Expected Authconfig to be empty, but was %v", authconfig)
}

func TestGetAuthConfigMissingResponse(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	client := mock_ecr.NewMockECRClient(ctrl)
	factory := mock_ecr.NewMockECRFactory(ctrl)

	authData := &apicontainer.ECRAuthData{
		Region:           "us-west-2",
		RegistryID:       "0123456789012",
		EndpointOverride: "my.endpoint",
	}
	proxyEndpoint := "proxy"

	provider := ecrAuthProvider{
		factory:    factory,
		tokenCache: async.NewLRUCache(tokenCacheSize, tokenCacheTTL),
	}

	registryAuthData := &apicontainer.RegistryAuthenticationData{
		ECRAuthData: authData,
	}

	factory.EXPECT().GetClient(authData).Return(client, nil)
	client.EXPECT().GetAuthorizationToken(authData.RegistryID)

	authconfig, err := provider.GetAuthconfig(proxyEndpoint+"/myimage", registryAuthData)
	if err == nil {
		t.Fatal("Expected error to be present, but was nil", err)
	}
	require.Error(t, err, "Expected error to be present, but was nil", err)
	assert.Equal(t, registry.AuthConfig{}, authconfig, "Expected Authconfig to be empty, but was %v", authconfig)
}

func TestGetAuthConfigECRError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	client := mock_ecr.NewMockECRClient(ctrl)
	factory := mock_ecr.NewMockECRFactory(ctrl)

	authData := &apicontainer.ECRAuthData{
		Region:           "us-west-2",
		RegistryID:       "0123456789012",
		EndpointOverride: "my.endpoint",
	}
	proxyEndpoint := "proxy"

	provider := ecrAuthProvider{
		factory:    factory,
		tokenCache: async.NewLRUCache(tokenCacheSize, tokenCacheTTL),
	}

	registryAuthData := &apicontainer.RegistryAuthenticationData{
		ECRAuthData: authData,
	}

	factory.EXPECT().GetClient(authData).Return(client, nil)
	client.EXPECT().GetAuthorizationToken(authData.RegistryID).Return(nil, errors.New("test error"))

	authconfig, err := provider.GetAuthconfig(proxyEndpoint+"/myimage", registryAuthData)
	require.Error(t, err, "Expected error to be present, but was nil", err)
	assert.Equal(t, registry.AuthConfig{}, authconfig, "Expected Authconfig to be empty, but was %v", authconfig)
}

func TestGetAuthConfigNoAuthData(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	factory := mock_ecr.NewMockECRFactory(ctrl)
	proxyEndpoint := "proxy"
	provider := ecrAuthProvider{
		factory:    factory,
		tokenCache: async.NewLRUCache(tokenCacheSize, tokenCacheTTL),
	}

	authconfig, err := provider.GetAuthconfig(proxyEndpoint+"/myimage", nil)
	require.Error(t, err, "Expected error to be present, but was nil", err)
	assert.Equal(t, registry.AuthConfig{}, authconfig, "Expected Authconfig to be empty, but was %v", authconfig)
}

func TestIsTokenValid(t *testing.T) {
	provider := ecrAuthProvider{}

	var testAuthTimes = []struct {
		expireIn time.Duration
		expected bool
	}{
		{-1 * time.Minute, false},
		{time.Duration(0), false},
		{1 * time.Minute, false},
		{MinimumJitterDuration, false},
		{MinimumJitterDuration*2 + (1 * time.Second), true},
	}

	for _, testCase := range testAuthTimes {
		testAuthData := &types.AuthorizationData{
			ProxyEndpoint:      aws.String(testProxyEndpoint),
			AuthorizationToken: aws.String(testToken),
			ExpiresAt:          aws.Time(time.Now().Add(testCase.expireIn)),
		}

		actual := provider.IsTokenValid(testAuthData)
		assert.Equal(t, testCase.expected, actual,
			fmt.Sprintf("Expected IsTokenValid to be %t, got %t: for expiraing at %s", testCase.expected, actual, testCase.expireIn))
	}
}

func TestAuthorizationTokenCacheMiss(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	factory := mock_ecr.NewMockECRFactory(ctrl)
	ecrClient := mock_ecr.NewMockECRClient(ctrl)
	mockCache := mock_async.NewMockCache(ctrl)

	provider := ecrAuthProvider{
		factory:    factory,
		tokenCache: mockCache,
	}
	username := "test_user"
	password := "test_passwd"

	proxyEndpoint := "proxy"
	authData := &apicontainer.ECRAuthData{
		Region:           "us-west-2",
		RegistryID:       "0123456789012",
		EndpointOverride: "my.endpoint",
	}
	authData.SetPullCredentials(credentials.IAMRoleCredentials{
		RoleArn: "arn:aws:iam::123456789012:role/test",
	})

	registryAuthData := &apicontainer.RegistryAuthenticationData{
		ECRAuthData: authData,
	}

	key := cacheKey{
		roleARN:          authData.GetPullCredentials().RoleArn,
		region:           authData.Region,
		registryID:       authData.RegistryID,
		endpointOverride: authData.EndpointOverride,
	}

	dockerAuthData := &types.AuthorizationData{
		ProxyEndpoint:      aws.String(proxyEndpointScheme + proxyEndpoint),
		AuthorizationToken: aws.String(base64.StdEncoding.EncodeToString([]byte(username + ":" + password))),
	}

	mockCache.EXPECT().Get(key.String()).Return(nil, false)
	factory.EXPECT().GetClient(authData).Return(ecrClient, nil)
	ecrClient.EXPECT().GetAuthorizationToken(authData.RegistryID).Return(dockerAuthData, nil)
	mockCache.EXPECT().Set(key.String(), dockerAuthData)

	authconfig, err := provider.GetAuthconfig(proxyEndpoint+"myimage", registryAuthData)
	assert.NoError(t, err)
	assert.Equal(t, username, authconfig.Username)
	assert.Equal(t, password, authconfig.Password)
}

func TestAuthorizationTokenCacheHit(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	factory := mock_ecr.NewMockECRFactory(ctrl)
	mockCache := mock_async.NewMockCache(ctrl)

	provider := ecrAuthProvider{
		factory:    factory,
		tokenCache: mockCache,
	}
	username := "test_user"
	password := "test_passwd"

	proxyEndpoint := "proxy"
	testAuthData := &types.AuthorizationData{
		ProxyEndpoint:      aws.String(proxyEndpointScheme + proxyEndpoint),
		AuthorizationToken: aws.String(base64.StdEncoding.EncodeToString([]byte(username + ":" + password))),
		ExpiresAt:          aws.Time(time.Now().Add(12 * time.Hour)),
	}
	authData := &apicontainer.ECRAuthData{
		Region:           "us-west-2",
		RegistryID:       "0123456789012",
		EndpointOverride: "my.endpoint",
	}

	registryAuthData := &apicontainer.RegistryAuthenticationData{
		ECRAuthData: authData,
	}

	key := cacheKey{
		region:           authData.Region,
		registryID:       authData.RegistryID,
		endpointOverride: authData.EndpointOverride,
	}

	mockCache.EXPECT().Get(key.String()).Return(testAuthData, true)
	authconfig, err := provider.GetAuthconfig(proxyEndpoint+"myimage", registryAuthData)
	assert.NoError(t, err)
	assert.Equal(t, username, authconfig.Username)
	assert.Equal(t, password, authconfig.Password)
}

func TestAuthorizationTokenCacheWithCredentialsHit(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	factory := mock_ecr.NewMockECRFactory(ctrl)
	mockCache := mock_async.NewMockCache(ctrl)

	provider := ecrAuthProvider{
		factory:    factory,
		tokenCache: mockCache,
	}
	username := "test_user"
	password := "test_passwd"

	proxyEndpoint := "proxy"
	testAuthData := &types.AuthorizationData{
		ProxyEndpoint:      aws.String(proxyEndpointScheme + proxyEndpoint),
		AuthorizationToken: aws.String(base64.StdEncoding.EncodeToString([]byte(username + ":" + password))),
		ExpiresAt:          aws.Time(time.Now().Add(12 * time.Hour)),
	}
	authData := &apicontainer.ECRAuthData{
		Region:           "us-west-2",
		RegistryID:       "0123456789012",
		EndpointOverride: "my.endpoint",
	}
	authData.SetPullCredentials(credentials.IAMRoleCredentials{
		RoleArn: "arn:aws:iam::123456789012:role/test",
	})

	registryAuthData := &apicontainer.RegistryAuthenticationData{
		ECRAuthData: authData,
	}

	key := cacheKey{
		roleARN:          authData.GetPullCredentials().RoleArn,
		region:           authData.Region,
		registryID:       authData.RegistryID,
		endpointOverride: authData.EndpointOverride,
	}

	mockCache.EXPECT().Get(key.String()).Return(testAuthData, true)
	authconfig, err := provider.GetAuthconfig(proxyEndpoint+"myimage", registryAuthData)
	assert.NoError(t, err)
	assert.Equal(t, username, authconfig.Username)
	assert.Equal(t, password, authconfig.Password)
}

func TestAuthorizationTokenCacheHitExpired(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	factory := mock_ecr.NewMockECRFactory(ctrl)
	ecrClient := mock_ecr.NewMockECRClient(ctrl)
	mockCache := mock_async.NewMockCache(ctrl)

	provider := ecrAuthProvider{
		factory:    factory,
		tokenCache: mockCache,
	}
	username := "test_user"
	password := "test_passwd"

	proxyEndpoint := "proxy"
	testAuthData := &types.AuthorizationData{
		ProxyEndpoint:      aws.String(proxyEndpointScheme + proxyEndpoint),
		AuthorizationToken: aws.String(base64.StdEncoding.EncodeToString([]byte(username + ":" + password))),
		ExpiresAt:          aws.Time(time.Now()),
	}
	authData := &apicontainer.ECRAuthData{
		Region:           "us-west-2",
		RegistryID:       "0123456789012",
		EndpointOverride: "my.endpoint",
	}
	authData.SetPullCredentials(credentials.IAMRoleCredentials{
		RoleArn: "arn:aws:iam::123456789012:role/test",
	})

	registryAuthData := &apicontainer.RegistryAuthenticationData{
		ECRAuthData: authData,
	}

	key := cacheKey{
		roleARN:          authData.GetPullCredentials().RoleArn,
		region:           authData.Region,
		registryID:       authData.RegistryID,
		endpointOverride: authData.EndpointOverride,
	}

	dockerAuthData := &types.AuthorizationData{
		ProxyEndpoint:      aws.String(proxyEndpointScheme + proxyEndpoint),
		AuthorizationToken: aws.String(base64.StdEncoding.EncodeToString([]byte(username + ":" + password))),
	}

	mockCache.EXPECT().Get(key.String()).Return(testAuthData, true)
	mockCache.EXPECT().Delete(key.String())
	factory.EXPECT().GetClient(authData).Return(ecrClient, nil)
	ecrClient.EXPECT().GetAuthorizationToken(authData.RegistryID).Return(dockerAuthData, nil)
	mockCache.EXPECT().Set(key.String(), dockerAuthData)

	authconfig, err := provider.GetAuthconfig(proxyEndpoint+"myimage", registryAuthData)
	assert.NoError(t, err)
	assert.Equal(t, username, authconfig.Username)
	assert.Equal(t, password, authconfig.Password)
}

func TestExtractECRTokenError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	factory := mock_ecr.NewMockECRFactory(ctrl)
	ecrClient := mock_ecr.NewMockECRClient(ctrl)
	mockCache := mock_async.NewMockCache(ctrl)

	provider := ecrAuthProvider{
		factory:    factory,
		tokenCache: mockCache,
	}
	username := "test_user"
	password := "test_passwd"

	proxyEndpoint := "proxy"
	testAuthData := &types.AuthorizationData{
		ProxyEndpoint: aws.String(proxyEndpointScheme + proxyEndpoint),
		// This will makes the extract fail
		AuthorizationToken: aws.String("-"),
		ExpiresAt:          aws.Time(time.Now().Add(1 * time.Hour)),
	}
	authData := &apicontainer.ECRAuthData{
		Region:           "us-west-2",
		RegistryID:       "0123456789012",
		EndpointOverride: "my.endpoint",
	}
	authData.SetPullCredentials(credentials.IAMRoleCredentials{
		RoleArn: "arn:aws:iam::123456789012:role/test",
	})

	registryAuthData := &apicontainer.RegistryAuthenticationData{
		ECRAuthData: authData,
	}

	key := cacheKey{
		roleARN:          authData.GetPullCredentials().RoleArn,
		region:           authData.Region,
		registryID:       authData.RegistryID,
		endpointOverride: authData.EndpointOverride,
	}

	dockerAuthData := &types.AuthorizationData{
		ProxyEndpoint:      aws.String(proxyEndpointScheme + proxyEndpoint),
		AuthorizationToken: aws.String(base64.StdEncoding.EncodeToString([]byte(username + ":" + password))),
	}

	mockCache.EXPECT().Get(key.String()).Return(testAuthData, true)
	mockCache.EXPECT().Delete(key.String())
	factory.EXPECT().GetClient(authData).Return(ecrClient, nil)
	ecrClient.EXPECT().GetAuthorizationToken(authData.RegistryID).Return(dockerAuthData, nil)
	mockCache.EXPECT().Set(key.String(), dockerAuthData)

	authconfig, err := provider.GetAuthconfig(proxyEndpoint+"myimage", registryAuthData)
	assert.NoError(t, err)
	assert.Equal(t, username, authconfig.Username)
	assert.Equal(t, password, authconfig.Password)
}
