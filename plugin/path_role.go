package artifactorysecrets

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/vault/sdk/framework"
	"github.com/hashicorp/vault/sdk/logical"
)

// schema for the creation of the role, this will map the fields coming in from the
// vault request field map
var createRoleSchema = map[string]*framework.FieldSchema{
	"name": {
		Type:        framework.TypeString,
		Description: "The name of the role to be created",
	},
	"token_ttl": {
		Type:        framework.TypeDurationSecond,
		Description: "The TTL of the token",
		Default:     600,
	},
	"max_ttl": {
		Type:        framework.TypeDurationSecond,
		Description: "The TTL of the token",
		Default:     3600,
	},
	"permission_targets": {
		Type:        framework.TypeString,
		Description: "List of permission target configurations",
	},
}

// remove the specified role from the storage
func (backend *ArtifactoryBackend) removeRole(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	roleName := data.Get("name").(string)
	if roleName == "" {
		return logical.ErrorResponse("Unable to remove, missing role name"), nil
	}

	// get the role to make sure it exists and to get the role id
	role, err := backend.getRoleEntry(ctx, req.Storage, roleName)
	if err != nil {
		return nil, err
	}
	if role == nil {
		return nil, nil
	}

	// garbage collect: artifactory group and associated permission targets
	// since permission targets are only created for this specific group, we must delete them when a group is deleted
	// ac, err := backend.getArtifactoryClient(ctx, req.Storage)

	cfg, err := backend.getConfig(ctx, req.Storage)
	if err != nil {
		return logical.ErrorResponse("failed to obtain artifactory config"), err
	}

	ac, err := backend.getClient(ctx, cfg)
	if err != nil {
		return logical.ErrorResponse("failed to obtain artifactory client"), err
	}
	if err = ac.DeleteGroup(role); err != nil {
		return nil, err
	}

	// Delete all permission targets
	for idx := range role.PermissionTargets {
		ptName := permissionTargetName(role, idx)
		if err := ac.DeletePermissionTarget(ptName); err != nil {
			return logical.ErrorResponse("failed to delete a permission target: ", ptName), err
		}
	}

	if err := backend.deleteRoleEntry(ctx, req.Storage, roleName); err != nil {
		return logical.ErrorResponse(fmt.Sprintf("Unable to remove role %s", roleName)), err
	}

	return &logical.Response{}, nil
}

// read the current role from the inputs and return it if it exists
func (backend *ArtifactoryBackend) readRole(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	roleName := data.Get("name").(string)
	role, err := backend.getRoleEntry(ctx, req.Storage, roleName)
	if err != nil {
		return logical.ErrorResponse("Error reading role"), err
	}

	if role == nil {
		return nil, nil
	}

	return &logical.Response{
		Data: map[string]interface{}{
			"name":               role.Name,
			"id":                 role.RoleID,
			"token_ttl":          int64(role.TokenTTL / time.Second),
			"max_ttl":            int64(role.MaxTTL / time.Second),
			"permission_targets": role.RawPermissionTargets,
		},
	}, nil
}

// read the current role from the inputs and return it if it exists
func (backend *ArtifactoryBackend) listRoles(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	roles, err := backend.listRoleEntries(ctx, req.Storage)
	if err != nil {
		return logical.ErrorResponse("Error listing roles"), err
	}
	return logical.ListResponse(roles), nil
}

func (backend *ArtifactoryBackend) createUpdateRole(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	roleName := data.Get("name").(string)
	if roleName == "" {
		return logical.ErrorResponse("Role name not supplied"), nil
	}

	role, err := backend.getRoleEntry(ctx, req.Storage, roleName)
	if err != nil {
		return logical.ErrorResponse("Error reading role"), err
	}

	cfg, err := backend.getConfig(ctx, req.Storage)
	if err != nil {
		return logical.ErrorResponse("failed to obtain artifactory config"), err
	}

	ac, err := backend.getClient(ctx, cfg)
	if err != nil {
		return logical.ErrorResponse("failed to obtain artifactory client"), err
	}

	if role == nil {
		// creating a new role
		role = &RoleStorageEntry{}
		// set the role ID
		roleID, _ := uuid.NewUUID()
		role.RoleID = roleID.String()
		role.Name = roleName

		if err := ac.CreateOrReplaceGroup(role); err != nil {
			return logical.ErrorResponse("failed to create an artifactory group - ", err.Error()), err
		}
	}

	if ttlRaw, ok := data.GetOk("token_ttl"); ok {
		role.TokenTTL = time.Duration(ttlRaw.(int)) * time.Second
	} else {
		role.TokenTTL = time.Duration(createRoleSchema["token_ttl"].Default.(int)) * time.Second
	}
	if maxttlRaw, ok := data.GetOk("max_ttl"); ok {
		role.MaxTTL = time.Duration(maxttlRaw.(int)) * time.Second
	} else {
		role.MaxTTL = time.Duration(createRoleSchema["max_ttl"].Default.(int)) * time.Second
	}

	// TODO: garbage collection - rollback operation
	//  - delete group if there's any error while creating a new permission target for a 'new' role
	//  - delete any newly created permission targets if role isn't saved
	if ptsRaw, ok := data.GetOk("permission_targets"); ok {
		role.RawPermissionTargets = ptsRaw.(string)

		newPts := []PermissionTarget{}
		err := json.Unmarshal([]byte(ptsRaw.(string)), &newPts)
		if err != nil {
			return logical.ErrorResponse("Error unmarshal permission targets. Expecting list of permission targets - " + err.Error()), err
		}

		// validate for all permission targets before creating and saving
		for _, pt := range newPts {
			err := pt.assertValid()
			if err != nil {
				return logical.ErrorResponse("Failed to validate a permission target - " + err.Error()), err
			}
		}

		existingPts := role.PermissionTargets
		role.PermissionTargets = newPts

		for idx, pt := range newPts {
			ptName := permissionTargetName(role, idx)
			if err := ac.CreateOrUpdatePermissionTarget(role, &pt, ptName); err != nil {
				return logical.ErrorResponse("Failed to create/update a permission target - ", err.Error()), err
			}
		}

		// garbage collect: delete excess permission targets
		// naive solution
		// This will be replaced with WAL rollback.
		if len(existingPts) > len(newPts) {
			for idx := range existingPts[len(newPts):] {
				ptName := permissionTargetName(role, idx)
				backend.Logger().Info("Deleting permission target from artifactory", "name", ptName)
				if err := ac.DeletePermissionTarget(ptName); err != nil {
					return logical.ErrorResponse("failed to delete a permission target - ", err.Error()), err
				}
			}
		}
	}

	if err := backend.setRoleEntry(ctx, req.Storage, *role); err != nil {
		return logical.ErrorResponse("Error saving role - " + err.Error()), err
	}

	roleDetails := map[string]interface{}{
		"role_id":            role.RoleID,
		"role_name":          role.Name,
		"permission_targets": role.RawPermissionTargets,
	}
	return &logical.Response{Data: roleDetails}, nil
}

// set up the paths for the roles within vault
func pathRole(backend *ArtifactoryBackend) []*framework.Path {
	paths := []*framework.Path{
		{
			Pattern: fmt.Sprintf("%s/%s", rolesPrefix, framework.GenericNameRegex("name")),
			Fields:  createRoleSchema,
			Callbacks: map[logical.Operation]framework.OperationFunc{
				logical.CreateOperation: backend.createUpdateRole,
				logical.UpdateOperation: backend.createUpdateRole,
				logical.ReadOperation:   backend.readRole,
				logical.DeleteOperation: backend.removeRole,
			},
			HelpSynopsis:    pathRoleHelpSyn,
			HelpDescription: pathRoleHelpDesc,
		},
	}

	return paths
}

func pathRoleList(backend *ArtifactoryBackend) []*framework.Path {
	// Paths for listing role sets
	paths := []*framework.Path{
		{
			Pattern: fmt.Sprintf("%s?/?", rolesPrefix),
			Callbacks: map[logical.Operation]framework.OperationFunc{
				logical.ListOperation: backend.listRoles,
			},
			HelpSynopsis: pathListRoleHelpSyn,
		},
	}
	return paths
}

const pathRoleHelpSyn = `Read/write sets of permission targets to be given to generated credentials for specified role.`
const pathRoleHelpDesc = `
This path allows you create roles, which bind sets of permission targets
to specific repositories with patterns and actinos. Secrets are generated 
under a role and will have the given set of permission targets on group.

The specified permission targets file accepts an JSON string
with the following format:

[
	{
		"repo": {
			"include_patterns": ["**"] (default),
			"exclude_patterns": [""] (default),
			"repositories": ["local-repo1", "local-repo2", "remote-repo1", "virtual-repo2"],
			"operations": ["read","annotate","write"]
		},
		"build": {
			"include_patterns": ["**"] (default),
			"exclude_patterns": [""] (default),
			"repositories": ["artifactory-build-info"], (default, can't be changed)
			"operations": ["manage","read","annotate"]
		},
	}
]

| field | subfield 				 | required |
| ----- | ---------------- | -------- |
| repo  | N/A      				 | false    | 
|  			| include_patterns | false    | 
|  			| exclude_patterns | false    | 
|  			| repositories	   | true   	| 
|  			| operations		   | true	    | 
| build | N/A      				 | false    | 
|  			| include_patterns | false    | 
|  			| exclude_patterns | false    | 
|  			| repositories	   | true   	| 
|  			| operations		   | true	    | 
`

const pathListRoleHelpSyn = `List existing roles.`