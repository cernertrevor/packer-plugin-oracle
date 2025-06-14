// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package oci

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/hashicorp/packer-plugin-sdk/uuid"
	"github.com/oracle/oci-go-sdk/v65/common"
	core "github.com/oracle/oci-go-sdk/v65/core"
)

// driverOCI implements the Driver interface and communicates with Oracle
// OCI.
type driverOCI struct {
	computeClient core.ComputeClient
	vcnClient     core.VirtualNetworkClient
	cfg           *Config
}

var retryPolicy = &common.RetryPolicy{
	MaximumNumberAttempts: 10,
	ShouldRetryOperation: func(res common.OCIOperationResponse) bool {
		var e common.ServiceError
		if errors.As(res.Error, &e) {
			switch e.GetHTTPStatusCode() {
			case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable:
				return true
			}
		}
		return false
	},
	NextDuration: func(res common.OCIOperationResponse) time.Duration {
		x := uint64(res.AttemptNumber)
		d := time.Duration(math.Pow(2, float64(atomic.LoadUint64(&x)))) * time.Second
		j := time.Duration(rand.Float64()*(2000)) * time.Millisecond
		w := d + j
		return w
	},
}

var requestMetadata = common.RequestMetadata{
	RetryPolicy: retryPolicy,
}

// NewDriverOCI Creates a new driverOCI with a connected compute client and a connected vcn client.
func NewDriverOCI(cfg *Config) (Driver, error) {
	coreClient, err := core.NewComputeClientWithConfigurationProvider(cfg.configProvider)
	if err != nil {
		return nil, err
	}

	vcnClient, err := core.NewVirtualNetworkClientWithConfigurationProvider(cfg.configProvider)
	if err != nil {
		return nil, err
	}

	return &driverOCI{
		computeClient: coreClient,
		vcnClient:     vcnClient,
		cfg:           cfg,
	}, nil
}

// CreateInstance creates a new compute instance.
func (d *driverOCI) CreateInstance(ctx context.Context, publicKey string) (string, error) {
	metadata := map[string]string{
		"ssh_authorized_keys": publicKey,
	}
	if d.cfg.Metadata != nil {
		for key, value := range d.cfg.Metadata {
			metadata[key] = value
		}
	}
	if d.cfg.UserData != "" {
		metadata["user_data"] = d.cfg.UserData
	}

	// Create VNIC details for instance
	CreateVnicDetails := core.CreateVnicDetails{
		AssignPublicIp:      d.cfg.CreateVnicDetails.AssignPublicIp,
		DisplayName:         d.cfg.CreateVnicDetails.DisplayName,
		HostnameLabel:       d.cfg.CreateVnicDetails.HostnameLabel,
		NsgIds:              d.cfg.CreateVnicDetails.NsgIds,
		PrivateIp:           d.cfg.CreateVnicDetails.PrivateIp,
		SkipSourceDestCheck: d.cfg.CreateVnicDetails.SkipSourceDestCheck,
		SubnetId:            d.cfg.CreateVnicDetails.SubnetId,
		DefinedTags:         d.cfg.CreateVnicDetails.DefinedTags,
		FreeformTags:        d.cfg.CreateVnicDetails.FreeformTags,
	}

	// Determine base image ID
	var imageId *string
	if d.cfg.BaseImageID != "" {
		imageId = &d.cfg.BaseImageID
	} else {
		request := core.ListImagesRequest{
			CompartmentId:          d.cfg.BaseImageFilter.CompartmentId,
			DisplayName:            d.cfg.BaseImageFilter.DisplayName,
			OperatingSystem:        d.cfg.BaseImageFilter.OperatingSystem,
			OperatingSystemVersion: d.cfg.BaseImageFilter.OperatingSystemVersion,
			Shape:                  d.cfg.BaseImageFilter.Shape,
			LifecycleState:         "AVAILABLE",
			SortBy:                 "TIMECREATED",
			SortOrder:              "DESC",
			RequestMetadata:        requestMetadata,
			Page:                   common.String(""),
		}

		for request.Page != nil && imageId == nil {
			// Pull images and determine which image ID to use, if BaseImageId not specified
			response, err := d.computeClient.ListImages(ctx, request)
			if err != nil {
				return "", err
			}

			if len(response.Items) == 0 && response.OpcNextPage == nil {
				return "", errors.New("base_image_filter returned no images")
			}

			if d.cfg.BaseImageFilter.DisplayNameSearch != nil {
				// Return most recent image that matches regex
				imageNameRegex, err := regexp.Compile(*d.cfg.BaseImageFilter.DisplayNameSearch)
				if err != nil {
					return "", err
				}
				for _, image := range response.Items {
					if imageNameRegex.MatchString(*image.DisplayName) {
						imageId = image.Id
						break
					}
				}

				if imageId == nil && response.OpcNextPage == nil {
					return "", errors.New("no image matched display_name_search criteria")
				}
			} else {
				// If no regex provided, simply return most recent image pulled
				if len(response.Items) > 0 {
					imageId = response.Items[0].Id
				}
			}

			request.Page = response.OpcNextPage
		}
	}

	// Create Source details which will be used to Launch Instance
	InstanceSourceDetails := core.InstanceSourceViaImageDetails{ImageId: imageId}

	if d.cfg.BootVolumeSizeInGBs != 0 {
		InstanceSourceDetails.BootVolumeSizeInGBs = &d.cfg.BootVolumeSizeInGBs
	}

	// Build instance details
	instanceDetails := core.LaunchInstanceDetails{
		AvailabilityDomain: &d.cfg.AvailabilityDomain,
		CompartmentId:      &d.cfg.CompartmentID,
		CreateVnicDetails:  &CreateVnicDetails,
		DefinedTags:        d.cfg.InstanceDefinedTags,
		DisplayName:        d.cfg.InstanceName,
		FreeformTags:       d.cfg.InstanceTags,
		Shape:              &d.cfg.Shape,
		SourceDetails:      InstanceSourceDetails,
		Metadata:           metadata,
	}

	if d.cfg.InstanceOptions.AreLegacyImdsEndpointsDisabled != nil {
		instanceDetails.InstanceOptions = &core.InstanceOptions{AreLegacyImdsEndpointsDisabled: d.cfg.InstanceOptions.AreLegacyImdsEndpointsDisabled}
	}

	if d.cfg.ShapeConfig.Ocpus != nil {
		LaunchInstanceShapeConfigDetails := core.LaunchInstanceShapeConfigDetails{
			Ocpus:       d.cfg.ShapeConfig.Ocpus,
			MemoryInGBs: d.cfg.ShapeConfig.MemoryInGBs,
		}

		if d.cfg.ShapeConfig.BaselineOcpuUtilization != nil {
			LaunchInstanceShapeConfigDetails.BaselineOcpuUtilization = core.LaunchInstanceShapeConfigDetailsBaselineOcpuUtilizationEnum(*d.cfg.ShapeConfig.BaselineOcpuUtilization)
		}

		instanceDetails.ShapeConfig = &LaunchInstanceShapeConfigDetails
	}

	instance, err := d.computeClient.LaunchInstance(context.TODO(), core.LaunchInstanceRequest{
		LaunchInstanceDetails: instanceDetails,
		RequestMetadata:       requestMetadata,
	})

	if err != nil {
		return "", err
	}

	return *instance.Id, nil
}

// CreateImage creates a new custom image.
func (d *driverOCI) CreateImage(ctx context.Context, id string) (core.Image, error) {
	res, err := d.computeClient.CreateImage(ctx, core.CreateImageRequest{CreateImageDetails: core.CreateImageDetails{
		CompartmentId: &d.cfg.ImageCompartmentID,
		InstanceId:    &id,
		DisplayName:   &d.cfg.ImageName,
		FreeformTags:  d.cfg.Tags,
		DefinedTags:   d.cfg.DefinedTags,
		LaunchMode:    core.CreateImageDetailsLaunchModeEnum(d.cfg.LaunchMode),
	},
		RequestMetadata: requestMetadata,
	})

	if err != nil {
		return core.Image{}, err
	}

	return res.Image, nil
}

// UpdateImageCapabilitySchema creates a new custom image.
func (d *driverOCI) UpdateImageCapabilitySchema(ctx context.Context, imageId string) (core.UpdateComputeImageCapabilitySchemaResponse, error) {

	// get the schema associated with the newly created image
	schema, err := d.computeClient.ListComputeImageCapabilitySchemas(context.Background(), core.ListComputeImageCapabilitySchemasRequest{
		ImageId: &imageId,
	})
	if err != nil {
		return core.UpdateComputeImageCapabilitySchemaResponse{}, err
	}
	// no schema found, need to download the global image schema, use it as a base to create an image schema for this image
	// and create the schema
	if len(schema.Items) < 1 {
		// get the global schema list
		globalSchemaList, err := d.computeClient.ListComputeGlobalImageCapabilitySchemas(context.Background(), core.ListComputeGlobalImageCapabilitySchemasRequest{})
		if err != nil {
			return core.UpdateComputeImageCapabilitySchemaResponse{}, err
		}
		if len(globalSchemaList.Items) < 1 {
			return core.UpdateComputeImageCapabilitySchemaResponse{}, errors.New("unable to find any global schemas")
		}

		// get the global schema based on ocid and latest version guid
		var globalSchemaId = globalSchemaList.Items[0].Id
		var globalSchemaCurrentVersion = globalSchemaList.Items[0].CurrentVersionName
		globalSchema, err := d.computeClient.GetComputeGlobalImageCapabilitySchemaVersion(context.Background(),
			core.GetComputeGlobalImageCapabilitySchemaVersionRequest{ComputeGlobalImageCapabilitySchemaId: globalSchemaId,
				ComputeGlobalImageCapabilitySchemaVersionName: globalSchemaCurrentVersion})
		if err != nil {
			return core.UpdateComputeImageCapabilitySchemaResponse{}, err
		}

		// update schema data to replace all instances of "source": "GLOBAL" with "source": "IMAGE"
		newSchemaData := make(map[string]core.ImageCapabilitySchemaDescriptor)
		for key := range globalSchema.SchemaData {
			val := globalSchema.SchemaData[key]
			if boolVal, ok := val.(core.BooleanImageCapabilitySchemaDescriptor); ok {
				newSchemaData[key] = core.BooleanImageCapabilitySchemaDescriptor{Source: "IMAGE", DefaultValue: boolVal.DefaultValue}
			}
			if enumIntVal, ok := val.(core.EnumIntegerImageCapabilityDescriptor); ok {
				destArr := make([]int, len(enumIntVal.Values))
				copy(destArr, enumIntVal.Values)
				newVal := core.EnumIntegerImageCapabilityDescriptor{Source: "IMAGE", DefaultValue: enumIntVal.DefaultValue, Values: destArr}
				newSchemaData[key] = newVal
			}
			if enumStringVal, ok := val.(core.EnumStringImageCapabilitySchemaDescriptor); ok {
				destArr := make([]string, len(enumStringVal.Values))
				copy(destArr, enumStringVal.Values)
				newVal := core.EnumStringImageCapabilitySchemaDescriptor{Source: "IMAGE", DefaultValue: enumStringVal.DefaultValue, Values: destArr}
				newSchemaData[key] = newVal
			}
		}

		// create a new schema for the image by duplication the global schema
		req := core.CreateComputeImageCapabilitySchemaRequest{CreateComputeImageCapabilitySchemaDetails: core.CreateComputeImageCapabilitySchemaDetails{
			DisplayName:   common.String(fmt.Sprintf("Default Image Capability Schema for %s", d.cfg.ImageName)),
			ImageId:       &imageId,
			SchemaData:    newSchemaData,
			CompartmentId: &d.cfg.ImageCompartmentID,
			ComputeGlobalImageCapabilitySchemaVersionName: globalSchema.ComputeGlobalImageCapabilitySchemaVersion.Name,
		},
			OpcRetryToken: common.String(uuid.TimeOrderedUUID()),
		}
		_, err = d.computeClient.CreateComputeImageCapabilitySchema(context.Background(), req)
		if err != nil {
			return core.UpdateComputeImageCapabilitySchemaResponse{}, err
		}

		// try to get the schema again, now it should be good
		schema, err = d.computeClient.ListComputeImageCapabilitySchemas(context.Background(),
			core.ListComputeImageCapabilitySchemasRequest{
				ImageId: &imageId,
			})
		if err != nil {
			return core.UpdateComputeImageCapabilitySchemaResponse{}, err
		}
	}

	// update the schema to add the new custom fields
	if d.cfg.LaunchMode != "" {
		schema.Items[0].SchemaData["Compute.LaunchMode"] = core.EnumStringImageCapabilitySchemaDescriptor{Values: []string{"NATIVE", "EMULATED", "PARAVIRTUALIZED", "CUSTOM"}, DefaultValue: &d.cfg.LaunchMode, Source: "IMAGE"}
	}
	if d.cfg.NicAttachmentType != "" {
		schema.Items[0].SchemaData["Network.AttachmentType"] = core.EnumStringImageCapabilitySchemaDescriptor{Values: []string{"E1000", "VFIO", "PARAVIRTUALIZED"}, DefaultValue: &d.cfg.NicAttachmentType, Source: "IMAGE"}
	}

	// update the new fields to the schema definition
	resp, err := d.computeClient.UpdateComputeImageCapabilitySchema(ctx,
		core.UpdateComputeImageCapabilitySchemaRequest{ComputeImageCapabilitySchemaId: schema.Items[0].Id,
			UpdateComputeImageCapabilitySchemaDetails: core.UpdateComputeImageCapabilitySchemaDetails{SchemaData: schema.Items[0].SchemaData,
				FreeformTags: d.cfg.Tags,
				DefinedTags:  d.cfg.DefinedTags,
			}})

	if err != nil {
		return resp, err
	}

	return resp, nil
}

// DeleteImage deletes a custom image.
func (d *driverOCI) DeleteImage(ctx context.Context, id string) error {
	_, err := d.computeClient.DeleteImage(ctx, core.DeleteImageRequest{
		ImageId:         &id,
		RequestMetadata: requestMetadata,
	})
	return err
}

// GetInstanceIP returns the public or private IP corresponding to the given instance id.
func (d *driverOCI) GetInstanceIP(ctx context.Context, id string) (string, error) {
	vnics, err := d.computeClient.ListVnicAttachments(ctx, core.ListVnicAttachmentsRequest{
		InstanceId:      &id,
		CompartmentId:   &d.cfg.CompartmentID,
		RequestMetadata: requestMetadata,
	})
	if err != nil {
		return "", err
	}

	if len(vnics.Items) == 0 {
		return "", errors.New("instance has zero VNICs")
	}

	vnic, err := d.vcnClient.GetVnic(ctx, core.GetVnicRequest{
		VnicId:          vnics.Items[0].VnicId,
		RequestMetadata: requestMetadata,
	})
	if err != nil {
		return "", fmt.Errorf("error getting VNIC details: %s", err)
	}

	if d.cfg.UsePrivateIP {
		return *vnic.PrivateIp, nil
	}

	if vnic.PublicIp == nil {
		return "", fmt.Errorf("error getting VNIC Public Ip for: %s", id)
	}

	return *vnic.PublicIp, nil
}

func (d *driverOCI) GetInstanceInitialCredentials(ctx context.Context, id string) (string, string, error) {
	credentials, err := d.computeClient.GetWindowsInstanceInitialCredentials(ctx, core.GetWindowsInstanceInitialCredentialsRequest{
		InstanceId:      &id,
		RequestMetadata: requestMetadata,
	})
	if err != nil {
		return "", "", err
	}

	return *credentials.InstanceCredentials.Username, *credentials.InstanceCredentials.Password, err
}

// TerminateInstance terminates a compute instance.
func (d *driverOCI) TerminateInstance(ctx context.Context, id string) error {
	_, err := d.computeClient.TerminateInstance(ctx, core.TerminateInstanceRequest{
		InstanceId:      &id,
		RequestMetadata: requestMetadata,
	})
	return err
}

// WaitForImageCreation waits for a provisioning custom image to reach the
// "AVAILABLE" state.
func (d *driverOCI) WaitForImageCreation(ctx context.Context, id string) error {
	return waitForResourceToReachState(
		func(string) (string, error) {
			image, err := d.computeClient.GetImage(ctx, core.GetImageRequest{
				ImageId:         &id,
				RequestMetadata: requestMetadata,
			})
			if err != nil {
				return "", err
			}
			return string(image.LifecycleState), nil
		},
		id,
		[]string{"PROVISIONING"},
		"AVAILABLE",
		0,             //Unlimited Retries
		5*time.Second, //5 second wait between retries
	)
}

// WaitForInstanceState waits for an instance to reach the a given terminal
// state.
func (d *driverOCI) WaitForInstanceState(ctx context.Context, id string, waitStates []string, terminalState string) error {
	return waitForResourceToReachState(
		func(string) (string, error) {
			instance, err := d.computeClient.GetInstance(ctx, core.GetInstanceRequest{
				InstanceId:      &id,
				RequestMetadata: requestMetadata,
			})
			if err != nil {
				return "", err
			}
			return string(instance.LifecycleState), nil
		},
		id,
		waitStates,
		terminalState,
		0,             //Unlimited Retries
		5*time.Second, //5 second wait between retries
	)
}

// WaitForResourceToReachState checks the response of a request through a
// polled get and waits until the desired state or until the max retried has
// been reached.
func waitForResourceToReachState(getResourceState func(string) (string, error), id string, waitStates []string, terminalState string, maxRetries int, waitDuration time.Duration) error {
	for i := 0; maxRetries == 0 || i < maxRetries; i++ {
		state, err := getResourceState(id)
		if err != nil {
			return err
		}

		if stringSliceContains(waitStates, state) {
			time.Sleep(waitDuration)
			continue
		} else if state == terminalState {
			return nil
		}
		return fmt.Errorf("unexpected resource state %q, expecting a waiting state %s or terminal state  %q ", state, waitStates, terminalState)
	}
	return fmt.Errorf("maximum number of retries (%d) exceeded; resource did not reach state %q", maxRetries, terminalState)
}

// stringSliceContains loops through a slice of strings returning a boolean
// based on whether a given value is contained in the slice.
func stringSliceContains(slice []string, value string) bool {
	for _, elem := range slice {
		if elem == value {
			return true
		}
	}
	return false
}
