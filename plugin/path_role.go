package artifactorysecrets

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	v1 "github.com/atlassian/go-artifactory/v2/artifactory/v1"
	v2 "github.com/atlassian/go-artifactory/v2/artifactory/v2"
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
		Description: "permission targets config.",
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

	// remove the role
	// TODO: garbage collect artifactory group and associated permission targets
	// since permission targets are only created for this specific group, we must delete them when a group is deleted
	c, err := backend.getArtifactoryClient(ctx, req.Storage)
	if err != nil {
		return logical.ErrorResponse("failed to obtain artifactory client"), err
	}
	_, _, err = c.V1.Security.DeleteGroup(ctx, groupName(role.RoleID))
	if err != nil {
		return nil, err
	}

	for _, pt := range role.PermissionTargets {
		exist, err := c.V2.Security.HasPermissionTarget(ctx, *permissionTargetName(role, *pt.Name))
		if err != nil {
			return logical.ErrorResponse("failed to obtain permission target"), err
		}
		if exist {
			_, err := c.V2.Security.DeletePermissionTarget(ctx, *permissionTargetName(role, *pt.Name))
			if err != nil {
				return logical.ErrorResponse("failed to delete permission target"), err
			}
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

	c, err := backend.getArtifactoryClient(ctx, req.Storage)
	if err != nil {
		return logical.ErrorResponse("failed to obtain artifactory client"), err
	}

	if role == nil {
		role = &RoleStorageEntry{}
		// creating a new role

		// set the role ID
		roleID, _ := uuid.NewUUID()
		role.RoleID = roleID.String()
		role.Name = roleName

		// TODO: create artifactory group
		// group name: vault-plugin.<role_id>
		n := groupName(roleID.String())
		desc := fmt.Sprintf("vault plugin group for %s", roleName)
		group := v1.Group{
			Name:        &n,
			Description: &desc,
		}

		_, err := c.V1.Security.CreateOrReplaceGroup(ctx, n, &group)
		if err != nil {
			return logical.ErrorResponse("Failed to create/update a group" + err.Error()), err
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

	// TODO: create permission targets in artifactory
	// garbage collect permission targets when it's updated/removed
	if ptRaw, ok := data.GetOk("permission_targets"); ok {
		role.RawPermissionTargets = ptRaw.(string)

		newPts := []v2.PermissionTarget{}
		err := json.Unmarshal([]byte(ptRaw.(string)), &newPts)
		if err != nil {
			return logical.ErrorResponse("Error unmarshal permission targets - " + err.Error()), err
		}

		existingPts := role.PermissionTargets
		role.PermissionTargets = newPts

		// for _, pt := range newPts {
		// ptName := permissionTargetName(role, *pt.Name)
		// modifiedPt := replaceGroupName(&pt, groupName(role.RoleID))
		// modifiedPt.Name = permissionTargetName(role, *ptName)

		// exist, err := c.V2.Security.HasPermissionTarget(ctx, *ptName)
		// if err != nil {
		// 	return logical.ErrorResponse("failed to obtain permission target - " + err.Error()), err
		// }

		// if !exist {
		// 	_, err := c.V2.Security.CreatePermissionTarget(ctx, *ptName, modifiedPt)
		// 	if err != nil {
		// 		return logical.ErrorResponse("failed to create permission target - " + err.Error()), err
		// 	}
		// } else {
		// 	_, err := c.V2.Security.UpdatePermissionTarget(ctx, *ptName, modifiedPt)
		// 	if err != nil {
		// 		return logical.ErrorResponse("failed to update permission target - " + err.Error()), err
		// 	}
		// }
		// }

		// delete removed permission targets
		// naive solution
		for _, existingPt := range existingPts {
			for _, newPt := range newPts {
				if existingPt.Name == newPt.Name {
					continue
				}
			}
			// existing permission target doesn't exist in new permission targets.
			exist, err := c.V2.Security.HasPermissionTarget(ctx, *permissionTargetName(role, *existingPt.Name))
			if err != nil {
				return logical.ErrorResponse("failed to obtain permission target"), err
			}
			if exist {
				_, err := c.V2.Security.DeletePermissionTarget(ctx, *permissionTargetName(role, *existingPt.Name))
				if err != nil {
					return logical.ErrorResponse("failed to delete permission target"), err
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

const pathRoleHelpSyn = `Read/write sets of permission targets to be given to generated credentials for specified group.`
const pathRoleHelpDesc = `
This path allows you create roles, which bind sets of permission targets
to specific GCP group. Secrets are generated under a role and will have the
given set of permission targets on group.

The specified permission targets file accepts an JSON string
with the following format:

[
	{
		"name": "name1",
		"repo": {
			"include-patterns": ["**"] (default),
			"exclude-patterns": [""] (default),
			"repositories": ["local-rep1", "local-rep2", "remote-rep1", "virtual-rep2"],
			"actions": {
				"users" : {
					"bob": ["read","write","manage"],
					"alice" : ["write","annotate", "read"]
				},
				"groups" : {
					"dev-leads" : ["manage","read","annotate"],
					"readers" : ["read"]
				}
			}
		}
	}
]
`

const pathListRoleHelpSyn = `List existing roles.`
