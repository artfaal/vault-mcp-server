// Copyright IBM Corp. 2025, 2026
// SPDX-License-Identifier: MPL-2.0

package kv

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/hashicorp/vault-mcp-server/pkg/client"
	"github.com/hashicorp/vault-mcp-server/pkg/utils"
	"github.com/hashicorp/vault/api"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	log "github.com/sirupsen/logrus"
)

// WriteSecretMetadata creates a tool for writing the custom metadata of a KV v2 secret
func WriteSecretMetadata(logger *log.Logger) server.ServerTool {
	return server.ServerTool{
		Tool: mcp.NewTool("write_secret_metadata",
			mcp.WithToolAnnotation(
				mcp.ToolAnnotation{
					DestructiveHint: utils.ToBoolPtr(true), // This is destructive because it overwrites the keys it is given if they already exist
					IdempotentHint:  utils.ToBoolPtr(true), // No new secret version is created, so repeating the same call leaves the same state
				},
			),
			mcp.WithDescription("Describes an existing secret on a KV v2 mount by writing its custom metadata, such as what the secret is for or who owns it. Use this after 'write_secret' to annotate the secret. Only the keys given are updated: any other custom metadata key keeps its value, and the remaining metadata fields ('max_versions', 'cas_required', 'delete_version_after') are left alone. No new version of the secret is created. Requires a KV v2 mount, an existing secret, and a policy granting the 'patch' capability on the metadata path of the secret."),
			mcp.WithString("mount",
				mcp.Required(),
				mcp.Description("The mount path of the secret engine. For example, if the secret is 'secrets/application/credentials', this should be 'secrets' without the trailing slash."),
			),
			mcp.WithString("path",
				mcp.Required(),
				mcp.Description("The full path of the secret without the mount prefix. For example, if the secret is 'secrets/application/credentials', this should be 'application/credentials'."),
			),
			mcp.WithObject("custom_metadata",
				mcp.Required(),
				mcp.MinProperties(1),
				mcp.MaxProperties(64), // Vault stores at most 64 custom metadata pairs per secret
				mcp.AdditionalProperties(map[string]any{"type": []string{"string", "null"}}),
				mcp.Description("The custom metadata keys to write, for example {\"description\": \"Token used by the billing job\"}. At least one key is required. Values must be strings, or null to remove a key that is already stored."),
			),
		),
		Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return writeSecretMetadataHandler(ctx, req, logger)
		},
	}
}

func writeSecretMetadataHandler(ctx context.Context, req mcp.CallToolRequest, logger *log.Logger) (*mcp.CallToolResult, error) {
	logger.Debug("Handling write_secret_metadata request")

	// Extract parameters
	args, ok := req.Params.Arguments.(map[string]interface{})
	if !ok {
		return mcp.NewToolResultError("Missing or invalid arguments format"), nil
	}

	mount, err := utils.ExtractMountPath(args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	path, ok := args["path"].(string)
	if !ok || path == "" {
		return mcp.NewToolResultError("Missing or invalid 'path' parameter"), nil
	}

	customMetadata, ok := args["custom_metadata"].(map[string]interface{})
	if !ok {
		return mcp.NewToolResultError("Missing or invalid 'custom_metadata' parameter, it must be an object such as {\"description\": \"Token used by the billing job\"}"), nil
	}

	// An empty object would be sent to Vault as a request to drop every custom
	// metadata key, which is never what the caller asked for.
	if len(customMetadata) == 0 {
		return mcp.NewToolResultError("The 'custom_metadata' object is empty, pass at least one key to write, or null as a value to remove a key"), nil
	}

	keys := make([]string, 0, len(customMetadata))
	for key, value := range customMetadata {
		switch value.(type) {
		case string, nil:
			keys = append(keys, key)
		default:
			return mcp.NewToolResultError(fmt.Sprintf("Invalid value for custom metadata key '%s', values must be strings, or null to remove a key", key)), nil
		}
	}
	// Keep the key order stable so the same call logs and answers the same way twice
	sort.Strings(keys)

	// Values can carry whatever the caller decided to describe the secret with, so only the key names are logged
	logger.WithFields(log.Fields{
		"mount": mount,
		"path":  path,
		"keys":  strings.Join(keys, ","),
	}).Debug("Writing secret metadata")

	// Get Vault client from context
	vault, err := client.GetVaultClientFromContext(ctx, logger)
	if err != nil {
		logger.WithError(err).Error("Failed to get Vault client")
		return mcp.NewToolResultError(fmt.Sprintf("Failed to get Vault client: %v", err)), nil
	}

	mounts, err := vault.Sys().ListMountsWithContext(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to list mounts: %v", err)), nil
	}

	// Check if the mount exists and is a KV v2 mount, as custom metadata only exists there
	m, ok := mounts[mount+"/"]
	if !ok {
		return mcp.NewToolResultError(fmt.Sprintf("mount path '%s' does not exist. Use 'create_mount' with the type kv2 to create the mount.", mount)), nil
	}
	if m.Options["version"] != "2" {
		return mcp.NewToolResultError(fmt.Sprintf("mount path '%s' is not a KV v2 mount and has no custom metadata. Only KV v2 secrets can be described this way.", mount)), nil
	}

	// PatchMetadata sends a JSON merge patch, so keys outside this call keep their
	// value; a POST here would replace the whole metadata object instead.
	err = vault.KVv2(mount).PatchMetadata(ctx, strings.TrimPrefix(path, "/"), api.KVMetadataPatchInput{
		CustomMetadata: customMetadata,
	})
	if err != nil {
		logger.WithError(err).WithFields(log.Fields{
			"mount": mount,
			"path":  path,
		}).Error("Failed to write secret metadata")

		// Only the two answers Vault gives for a reason the caller can act on get a hint,
		// anything else (a timeout, a rejected value) would be misdiagnosed by one
		hint := ""
		var respErr *api.ResponseError
		if errors.As(err, &respErr) {
			switch respErr.StatusCode {
			case http.StatusForbidden:
				hint = fmt.Sprintf(" The policy must grant the 'patch' capability on '%s/metadata/%s'.", mount, strings.TrimPrefix(path, "/"))
			case http.StatusNotFound:
				hint = " The secret must exist before its metadata can be written, use 'write_secret' first."
			}
		}
		return mcp.NewToolResultError(fmt.Sprintf("Failed to write the custom metadata of the secret at path '%s' in mount '%s': %v.%s", path, mount, err, hint)), nil
	}

	logger.WithFields(log.Fields{
		"mount": mount,
		"path":  path,
	}).Info("Successfully wrote secret metadata")

	return mcp.NewToolResultText(fmt.Sprintf("Successfully wrote the custom metadata keys '%s' of the secret at path '%s' in mount '%s'", strings.Join(keys, "', '"), path, mount)), nil
}
