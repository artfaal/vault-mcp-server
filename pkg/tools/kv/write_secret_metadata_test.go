// Copyright IBM Corp. 2025, 2026
// SPDX-License-Identifier: MPL-2.0

package kv

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeSecretMetadataRequest(customMetadata map[string]interface{}) mcp.CallToolRequest {
	return mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "write_secret_metadata",
			Arguments: map[string]interface{}{
				"mount":           "secret",
				"path":            "app/config",
				"custom_metadata": customMetadata,
			},
		},
	}
}

func TestWriteSecretMetadataHandler_PatchesCustomMetadata(t *testing.T) {
	logger := newLogger()
	var capturedMethod, capturedContentType string
	var capturedBody map[string]interface{}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sys/mounts", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, mountsV2Response("secret"))
	})
	mux.HandleFunc("/v1/secret/metadata/app/config", func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedContentType = r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&capturedBody)
		w.WriteHeader(http.StatusNoContent)
	})

	ctx, cleanup := newTestContext(t, mux)
	defer cleanup()

	req := writeSecretMetadataRequest(map[string]interface{}{
		"description": "Token used by the billing job",
		"owner":       nil,
	})

	result, err := writeSecretMetadataHandler(ctx, req, logger)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError, "expected success, got error: %s", getResultText(result))

	// A JSON merge patch keeps the metadata fields that are not part of the request,
	// a POST to the same path would replace all of them
	assert.Equal(t, http.MethodPatch, capturedMethod)
	assert.Equal(t, "application/merge-patch+json", capturedContentType)

	require.NotNil(t, capturedBody, "expected a write to Vault")
	customMetadata, ok := capturedBody["custom_metadata"].(map[string]interface{})
	require.True(t, ok, "written body should have a 'custom_metadata' object")
	assert.Equal(t, "Token used by the billing job", customMetadata["description"])
	assert.Contains(t, customMetadata, "owner", "a null value should be sent so Vault removes the key")
	assert.Nil(t, customMetadata["owner"])
}

func TestWriteSecretMetadataHandler_InvalidCustomMetadata(t *testing.T) {
	tests := []struct {
		name           string
		customMetadata map[string]interface{}
		expectedError  string
	}{
		{
			name:           "empty object",
			customMetadata: map[string]interface{}{},
			expectedError:  "is empty",
		},
		{
			name:           "number value",
			customMetadata: map[string]interface{}{"max_age": 30},
			expectedError:  "key 'max_age', values must be strings",
		},
		{
			name:           "nested object value",
			customMetadata: map[string]interface{}{"owner": map[string]interface{}{"team": "billing"}},
			expectedError:  "values must be strings",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := newLogger()
			requests := 0

			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.WriteHeader(http.StatusNoContent)
			})

			ctx, cleanup := newTestContext(t, mux)
			defer cleanup()

			result, err := writeSecretMetadataHandler(ctx, writeSecretMetadataRequest(tt.customMetadata), logger)
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.True(t, result.IsError, "expected an error for %s", tt.name)
			assert.Contains(t, getResultText(result), tt.expectedError)
			assert.Zero(t, requests, "invalid input should not reach Vault")
		})
	}
}

func TestWriteSecretMetadataHandler_PermissionDenied(t *testing.T) {
	logger := newLogger()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sys/mounts", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, mountsV2Response("secret"))
	})
	mux.HandleFunc("/v1/secret/metadata/app/config", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		jsonResponse(w, map[string]interface{}{"errors": []string{"1 error occurred:\n\t* permission denied\n\n"}})
	})

	ctx, cleanup := newTestContext(t, mux)
	defer cleanup()

	req := writeSecretMetadataRequest(map[string]interface{}{"description": "Token used by the billing job"})

	result, err := writeSecretMetadataHandler(ctx, req, logger)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.IsError, "a token without the 'patch' capability should surface an error")
	assert.Contains(t, getResultText(result), "permission denied")
	assert.Contains(t, getResultText(result), "'patch' capability")
}

func TestWriteSecretMetadataHandler_RejectedByVault(t *testing.T) {
	logger := newLogger()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sys/mounts", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, mountsV2Response("secret"))
	})
	mux.HandleFunc("/v1/secret/metadata/app/config", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		jsonResponse(w, map[string]interface{}{"errors": []string{"custom_metadata value exceeds 512 bytes"}})
	})

	ctx, cleanup := newTestContext(t, mux)
	defer cleanup()

	req := writeSecretMetadataRequest(map[string]interface{}{"description": "Token used by the billing job"})

	result, err := writeSecretMetadataHandler(ctx, req, logger)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.IsError)
	// An error Vault gave for another reason must not be diagnosed as a missing capability
	assert.NotContains(t, getResultText(result), "capability")
	assert.NotContains(t, getResultText(result), "write_secret")
}

func TestWriteSecretMetadataHandler_NotKVv2(t *testing.T) {
	logger := newLogger()
	metadataRequests := 0

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sys/mounts", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, map[string]interface{}{
			"data": map[string]interface{}{
				"secret/": map[string]interface{}{"type": "kv"},
			},
		})
	})
	mux.HandleFunc("/v1/secret/metadata/app/config", func(w http.ResponseWriter, r *http.Request) {
		metadataRequests++
		w.WriteHeader(http.StatusNoContent)
	})

	ctx, cleanup := newTestContext(t, mux)
	defer cleanup()

	req := writeSecretMetadataRequest(map[string]interface{}{"description": "Token used by the billing job"})

	result, err := writeSecretMetadataHandler(ctx, req, logger)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.IsError, "a KV v1 mount has no custom metadata")
	assert.Contains(t, getResultText(result), "not a KV v2 mount")
	assert.Zero(t, metadataRequests, "a KV v1 mount should not be written to")
}
