package vsphere

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform/helper/schema"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/vic/pkg/vsphere/tags"
)

// A list of valid object types for tagging are below. These are referenced by
// various helpers and tests.
const (
	vSphereTagTypeFolder                         = "Folder"
	vSphereTagTypeClusterComputeResource         = "ClusterComputeResource"
	vSphereTagTypeDatacenter                     = "Datacenter"
	vSphereTagTypeDatastore                      = "Datastore"
	vSphereTagTypeStoragePod                     = "StoragePod"
	vSphereTagTypeDistributedVirtualPortgroup    = "DistributedVirtualPortgroup"
	vSphereTagTypeDistributedVirtualSwitch       = "DistributedVirtualSwitch"
	vSphereTagTypeVmwareDistributedVirtualSwitch = "VmwareDistributedVirtualSwitch"
	vSphereTagTypeHostSystem                     = "HostSystem"
	vSphereTagTypeContentLibrary                 = "com.vmware.content.Library"
	vSphereTagTypeContentLibraryItem             = "com.vmware.content.library.Item"
	vSphereTagTypeHostNetwork                    = "HostNetwork"
	vSphereTagTypeNetwork                        = "Network"
	vSphereTagTypeOpaqueNetwork                  = "OpaqueNetwork"
	vSphereTagTypeResourcePool                   = "ResourcePool"
	vSphereTagTypeVirtualApp                     = "VirtualApp"
	vSphereTagTypeVirtualMachine                 = "VirtualMachine"

	vSphereTagTypeAll = "All"
)

// The following groups are type groups that are associated with the same type
// selection in the vSphere Client tag category UI.
var (
	// vSphereTagTypesForDistributedVirtualSwitch represents
	// types for virtual switches.
	vSphereTagTypesForDistributedVirtualSwitch = []string{
		vSphereTagTypeDistributedVirtualSwitch,
		vSphereTagTypeVmwareDistributedVirtualSwitch,
	}

	// vSphereTagTypesForNetwork represents the types for
	// networks.
	vSphereTagTypesForNetwork = []string{
		vSphereTagTypeHostNetwork,
		vSphereTagTypeNetwork,
		vSphereTagTypeOpaqueNetwork,
	}
)

// vSphereTagCategorySearchErrMultiple is an error message format for a tag
// category search that returned multiple results. This is a bug and needs to
// be reported so we can adjust the API.
const vSphereTagCategorySearchErrMultiple = `
Category name %q returned multiple results!

This is a bug - please report it at:
https://github.com/terraform-providers/terraform-provider-vsphere/issues

This version of the provider requires unique category names. To work around
this issue, please use a category name unique within your vCenter system.
`

// vSphereTagSearchErrMultiple is an error message format for a tag search that
// returned multiple results. This is a bug and needs to be reported so we can
// adjust the API.
const vSphereTagSearchErrMultiple = `
Tag name %q returned multiple results!

This is a bug - please report it at:
https://github.com/terraform-providers/terraform-provider-vsphere/issues

This version of the provider requires unique tag names. To work around
this issue, please use a tag name unique within your vCenter system.
`

// vSphereTagAttributeKey is the string key that should always be used as the
// argument to pass tags in to. Various resource tag helpers will depend on
// this value being consistent across resources.
//
// When adding tags to a resource schema, the easiest way to do that (for now)
// will be to use the following line:
//
//   vSphereTagAttributeKey: tagsSchema(),
//
// This will ensure that the correct key and schema is used across all resources.
const vSphereTagAttributeKey = "tags"

// tagsMinVersion is the minimum vSphere version required for tags.
var tagsMinVersion = vSphereVersion{
	product: "VMware vCenter Server",
	major:   6,
	minor:   0,
	patch:   0,
	build:   2559268,
}

// isEligibleTagEndpoint is a meta-validation that is used on login to see if
// the connected endpoint supports the CIS REST API, which we use for tags.
func isEligibleTagEndpoint(client *govmomi.Client) bool {
	if err := validateVirtualCenter(client); err != nil {
		return false
	}
	clientVer := parseVersionFromClient(client)
	if !clientVer.ProductEqual(tagsMinVersion) || clientVer.Older(tagsMinVersion) {
		return false
	}
	return true
}

// tagCategoryByName locates a tag category by name. It's used by the
// vsphere_tag_category data source, and the resource importer.
func tagCategoryByName(client *tags.RestClient, name string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultAPITimeout)
	defer cancel()
	cats, err := client.GetCategoriesByName(ctx, name)
	if err != nil {
		return "", fmt.Errorf("could not get category for name %q: %s", name, err)
	}

	if len(cats) < 1 {
		return "", fmt.Errorf("category name %q not found", name)
	}
	if len(cats) > 1 {
		// Although GetCategoriesByName does not seem to think that tag categories
		// are unique, empirical observation via the console and API show that they
		// are. If for some reason the returned results includes more than one ID,
		// we give an error, indicating that this is a bug and the user should
		// submit an issue.
		return "", fmt.Errorf(vSphereTagCategorySearchErrMultiple, name)
	}

	return cats[0].ID, nil
}

// tagByName locates a tag by it supplied name and category ID. Use
// tagCategoryByName to get the tag category ID if require the category ID as
// well.
func tagByName(client *tags.RestClient, name, categoryID string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultAPITimeout)
	defer cancel()
	tags, err := client.GetTagByNameForCategory(ctx, name, categoryID)
	if err != nil {
		return "", fmt.Errorf("could not get tag for name %q: %s", name, err)
	}

	if len(tags) < 1 {
		return "", fmt.Errorf("tag name %q not found in category ID %q", name, categoryID)
	}
	if len(tags) > 1 {
		// This situation is very similar to the one in tagCategoryByName. The API
		// docs even say that tags need to be unique in categories, yet
		// GetTagByNameForCategory still returns multiple results.
		return "", fmt.Errorf(vSphereTagSearchErrMultiple, name)
	}

	return tags[0].ID, nil
}

// tagsSchema returns the schema for the tags configuration attribute for each
// resource that needs it.
//
// The key is usually "tags" and should be a list of tag IDs to associate with
// this resource.
func tagsSchema() *schema.Schema {
	return &schema.Schema{
		Type:        schema.TypeSet,
		Description: "A list of tag IDs to apply to this object.",
		Optional:    true,
		Elem:        &schema.Schema{Type: schema.TypeString},
	}
}

// tagTypeForObject takes an object.Reference and returns the tag type based on
// its underlying type. If it's not in this list, we don't support it for
// tagging and we return an error.
func tagTypeForObject(obj object.Reference) (string, error) {
	switch obj.(type) {
	case *object.VirtualMachine:
		return vSphereTagTypeVirtualMachine, nil
	case *object.Datastore:
		return vSphereTagTypeDatastore, nil
	case *object.Network:
		return vSphereTagTypeNetwork, nil
	case *object.Folder:
		return vSphereTagTypeFolder, nil
	case *object.VmwareDistributedVirtualSwitch:
		return vSphereTagTypeVmwareDistributedVirtualSwitch, nil
	case *object.DistributedVirtualSwitch:
		return vSphereTagTypeDistributedVirtualSwitch, nil
	case *object.DistributedVirtualPortgroup:
		return vSphereTagTypeDistributedVirtualPortgroup, nil
	case *object.Datacenter:
		return vSphereTagTypeDatacenter, nil
	case *object.ClusterComputeResource:
		return vSphereTagTypeClusterComputeResource, nil
	case *object.HostSystem:
		return vSphereTagTypeHostSystem, nil
	}
	return "", fmt.Errorf("unsupported type for tagging: %T", obj)
}

// readTagsForResource reads the tags for a given reference and saves the list
// in the supplied ResourceData. It returns an error if there was an issue
// reading the tags.
func readTagsForResource(client *tags.RestClient, obj object.Reference, d *schema.ResourceData) error {
	objID := obj.Reference().Value
	objType, err := tagTypeForObject(obj)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultAPITimeout)
	defer cancel()
	ids, err := client.ListAttachedTags(ctx, objID, objType)
	if err != nil {
		return err
	}
	if err := d.Set(vSphereTagAttributeKey, ids); err != nil {
		return fmt.Errorf("error saving tag IDs to resource data: %s", err)
	}
	return nil
}

// tagDiffProcessor is an object that wraps the "complex" adding and removal of
// tags from an object.
type tagDiffProcessor struct {
	// The client connection.
	client *tags.RestClient

	// The object that is the subject of the tag addition and removal operations.
	subject object.Reference

	// A list of old (current) tags attached to the subject.
	oldTagIDs []string

	// The list of tags that should be attached to the subject.
	newTagIDs []string
}

// diffOldNew returns any elements of old that were missing in new.
func (p *tagDiffProcessor) diffOldNew() []string {
	return p.diff(p.oldTagIDs, p.newTagIDs)
}

// diffNewOld returns any elements of new that were missing in old.
func (p *tagDiffProcessor) diffNewOld() []string {
	return p.diff(p.newTagIDs, p.oldTagIDs)
}

// diff is what diffOldNew and diffNewOld hand off to.
func (p *tagDiffProcessor) diff(a, b []string) []string {
	var found bool
	c := make([]string, 0)
	for _, v1 := range a {
		for _, v2 := range b {
			if v1 == v2 {
				found = true
			}
		}
		if !found {
			c = append(c, v1)
		}
	}
	return c
}

// processAttachOperations processes all pending attach operations by diffing old
// and new and adding any IDs that were not found in old.
func (p *tagDiffProcessor) processAttachOperations() error {
	tagIDs := p.diffNewOld()
	if len(tagIDs) < 1 {
		// Nothing to do
		return nil
	}
	for _, tagID := range tagIDs {
		objID := p.subject.Reference().Value
		objType, err := tagTypeForObject(p.subject)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), defaultAPITimeout)
		defer cancel()
		if err := p.client.AttachTagToObject(ctx, tagID, objID, objType); err != nil {
			return err
		}
	}
	return nil
}

// processDetachOperations processes all pending detach operations by diffing
// new and old, and removing any IDs that were not found in new.
func (p *tagDiffProcessor) processDetachOperations() error {
	tagIDs := p.diffOldNew()
	if len(tagIDs) < 1 {
		// Nothing to do
		return nil
	}
	for _, tagID := range tagIDs {
		objID := p.subject.Reference().Value
		objType, err := tagTypeForObject(p.subject)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), defaultAPITimeout)
		defer cancel()
		if err := p.client.DetachTagFromObject(ctx, tagID, objID, objType); err != nil {
			return err
		}
	}
	return nil
}

// tagsClientIfDefined goes through the client validation process and returns
// the tags client only if there are tags defined in the supplied ResourceData.
//
// This should be used to fetch the tagging REST client on resources that
// support tags, usually closer to the beginning of a CRUD function to check to
// make sure it's worth proceeding with most of the operation. The returned
// client should be checked for nil before passing it to processTagDiff.
func tagsClientIfDefined(d *schema.ResourceData, meta interface{}) (*tags.RestClient, error) {
	if _, ok := d.GetOk(vSphereTagAttributeKey); ok {
		client, err := meta.(*VSphereClient).TagsClient()
		if err != nil {
			return nil, err
		}
		return client, nil
	}
	return nil, nil
}

// processTagDiff wraps the whole tag diffing operation into a nice clean
// function that resources can use.
func processTagDiff(client *tags.RestClient, d *schema.ResourceData, obj object.Reference) error {
	old, new := d.GetChange(vSphereTagAttributeKey)
	tdp := &tagDiffProcessor{
		client:    client,
		subject:   obj,
		oldTagIDs: sliceInterfacesToStrings(old.(*schema.Set).List()),
		newTagIDs: sliceInterfacesToStrings(new.(*schema.Set).List()),
	}
	if err := tdp.processDetachOperations(); err != nil {
		return fmt.Errorf("error detaching tags to object ID %q: %s", obj.Reference().Value, err)
	}
	if err := tdp.processAttachOperations(); err != nil {
		return fmt.Errorf("error attaching tags to object ID %q: %s", obj.Reference().Value, err)
	}
	return nil
}
