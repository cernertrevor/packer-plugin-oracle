// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package oci

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io/ioutil"
	"os"
	"strings"
	"testing"

	"github.com/go-ini/ini"
)

func testConfig(accessConfFile *os.File) map[string]interface{} {
	return map[string]interface{}{

		"availability_domain": "aaaa:PHX-AD-3",
		"access_cfg_file":     accessConfFile.Name(),

		// Image
		"base_image_ocid": "ocd1...",
		"image_name":      "HelloWorld",

		// Networking
		"subnet_ocid": "ocd1...",

		// Comm
		"ssh_username":   "opc",
		"use_private_ip": false,
		"metadata": map[string]string{
			"key": "value",
		},
		"defined_tags": map[string]map[string]interface{}{
			"namespace": {"key": "value"},
		},

		// Instance Details
		"instance_name": "hello-world",
		"instance_tags": map[string]string{
			"key": "value",
		},
		"create_vnic_details": map[string]interface{}{
			"nsg_ids": []string{"ocd1..."},
		},
		"shape":     "VM.Standard1.1",
		"disk_size": 60,
	}
}

func TestConfig(t *testing.T) {
	// Shared set-up and deferred deletion

	cfg, keyFile, err := baseTestConfigWithTmpKeyFile()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(keyFile.Name())

	cfgFile, err := writeTestConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(cfgFile.Name())

	// Temporarily set $HOME to temp directory to bypass default
	// access config loading.

	tmpHome, err := ioutil.TempDir("", "packer_config_test")
	if err != nil {
		t.Fatalf("Unexpected error when creating temporary directory: %+v", err)
	}
	defer os.Remove(tmpHome)

	home := os.Getenv("HOME")
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", home)

	// Config tests
	t.Run("BaseConfig", func(t *testing.T) {
		raw := testConfig(cfgFile)
		var c Config
		errs := c.Prepare(raw)

		if errs != nil {
			t.Fatalf("Unexpected error in configuration %+v", errs)
		}
	})

	t.Run("BaseImageFilterWithoutOCID", func(t *testing.T) {
		raw := testConfig(cfgFile)
		raw["base_image_ocid"] = ""
		raw["base_image_filter"] = map[string]interface{}{
			"display_name": "hello_world",
		}

		var c Config
		errs := c.Prepare(raw)

		if errs != nil {
			t.Fatalf("Unexpected error in configuration %+v", errs)
		}
	})

	t.Run("BaseImageFilterDefault", func(t *testing.T) {
		raw := testConfig(cfgFile)

		var c Config
		errs := c.Prepare(raw)
		if errs != nil {
			t.Fatalf("Unexpected error in configuration %+v", errs)
		}

		if *c.BaseImageFilter.Shape != raw["shape"] {
			t.Fatalf("Default base_image_filter shape %v does not equal config shape %v",
				*c.BaseImageFilter.Shape, raw["shape"])
		}
	})

	t.Run("LaunchMode", func(t *testing.T) {
		raw := testConfig(cfgFile)
		raw["image_launch_mode"] = "NATIVE"

		var c Config
		errs := c.Prepare(raw)
		if errs != nil {
			t.Fatalf("Unexpected error in configuration %+v", errs)
		}
	})

	t.Run("NicAttachmentType", func(t *testing.T) {
		raw := testConfig(cfgFile)
		raw["nic_attachment_type"] = "VFIO"

		var c Config
		errs := c.Prepare(raw)
		if errs != nil {
			t.Fatalf("Unexpected error in configuration %+v", errs)
		}
	})

	t.Run("NoAccessConfig", func(t *testing.T) {
		raw := testConfig(cfgFile)
		raw["access_cfg_file"] = "/tmp/random/access/config/file/should/not/exist"

		var c Config
		errs := c.Prepare(raw)

		expectedErrors := []string{
			"'user_ocid'", "'tenancy_ocid'", "'fingerprint'", "'key_file'",
		}

		if errs == nil {
			t.Fatalf("Expected errors %q but got none", expectedErrors)
		}

		s := errs.Error()
		for _, expected := range expectedErrors {
			if !strings.Contains(s, expected) {
				t.Errorf("Expected %q to contain '%s'", s, expected)
			}
		}
	})

	t.Run("AccessConfigTemplateOnly", func(t *testing.T) {
		raw := testConfig(cfgFile)
		delete(raw, "access_cfg_file")
		raw["user_ocid"] = "ocid1..."
		raw["tenancy_ocid"] = "ocid1..."
		raw["fingerprint"] = "00:00..."
		raw["key_file"] = keyFile.Name()

		var c Config
		errs := c.Prepare(raw)

		if errs != nil {
			t.Fatalf("err: %+v", errs)
		}

	})

	t.Run("TenancyReadFromAccessCfgFile", func(t *testing.T) {
		raw := testConfig(cfgFile)
		var c Config
		errs := c.Prepare(raw)
		if errs != nil {
			t.Fatalf("Unexpected error in configuration %+v", errs)
		}

		tenancy, err := c.configProvider.TenancyOCID()
		if err != nil {
			t.Fatalf("Unexpected error getting tenancy ocid: %v", err)
		}

		expected := "ocid1.tenancy.oc1..aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		if tenancy != expected {
			t.Errorf("Expected tenancy: %s, got %s.", expected, tenancy)
		}

	})

	t.Run("RegionNotDefaultedToPHXWhenSetInOCISettings", func(t *testing.T) {
		raw := testConfig(cfgFile)
		var c Config
		errs := c.Prepare(raw)
		if errs != nil {
			t.Fatalf("Unexpected error in configuration %+v", errs)
		}

		region, err := c.configProvider.Region()
		if err != nil {
			t.Fatalf("Unexpected error getting region: %v", err)
		}

		expected := "us-ashburn-1"
		if region != expected {
			t.Errorf("Expected region: %s, got %s.", expected, region)
		}

	})

	// Test the correct errors are produced when required template keys are
	// omitted.
	requiredKeys := []string{"availability_domain", "base_image_ocid", "shape", "subnet_ocid"}
	for _, k := range requiredKeys {
		t.Run(k+"_required", func(t *testing.T) {
			raw := testConfig(cfgFile)
			delete(raw, k)

			var c Config
			errs := c.Prepare(raw)

			if !strings.Contains(errs.Error(), k) {
				t.Errorf("Expected '%s' to contain '%s'", errs.Error(), k)
			}
		})
	}

	t.Run("ImageNameDefaultedIfEmpty", func(t *testing.T) {
		raw := testConfig(cfgFile)
		delete(raw, "image_name")

		var c Config
		errs := c.Prepare(raw)
		if errs != nil {
			t.Fatalf("Unexpected error in configuration %+v", errs)
		}

		if !strings.Contains(c.ImageName, "packer-") {
			t.Errorf("got default ImageName %q, want image name 'packer-{{timestamp}}'", c.ImageName)
		}
	})

	t.Run("user_ocid_overridden", func(t *testing.T) {
		expected := "override"
		raw := testConfig(cfgFile)
		raw["user_ocid"] = expected

		var c Config
		errs := c.Prepare(raw)
		if errs != nil {
			t.Fatalf("Unexpected error in configuration %+v", errs)
		}

		user, _ := c.configProvider.UserOCID()
		if user != expected {
			t.Errorf("Expected ConfigProvider.UserOCID: %s, got %s", expected, user)
		}
	})

	t.Run("tenancy_ocid_overidden", func(t *testing.T) {
		expected := "override"
		raw := testConfig(cfgFile)
		raw["tenancy_ocid"] = expected

		var c Config
		errs := c.Prepare(raw)
		if errs != nil {
			t.Fatalf("Unexpected error in configuration %+v", errs)
		}

		tenancy, _ := c.configProvider.TenancyOCID()
		if tenancy != expected {
			t.Errorf("Expected ConfigProvider.TenancyOCID: %s, got %s", expected, tenancy)
		}
	})

	t.Run("region_overidden", func(t *testing.T) {
		expected := "override"
		raw := testConfig(cfgFile)
		raw["region"] = expected

		var c Config
		errs := c.Prepare(raw)
		if errs != nil {
			t.Fatalf("Unexpected error in configuration %+v", errs)
		}

		region, _ := c.configProvider.Region()
		if region != expected {
			t.Errorf("Expected ConfigProvider.Region: %s, got %s", expected, region)
		}
	})

	t.Run("fingerprint_overidden", func(t *testing.T) {
		expected := "override"
		raw := testConfig(cfgFile)
		raw["fingerprint"] = expected

		var c Config
		errs := c.Prepare(raw)
		if errs != nil {
			t.Fatalf("Unexpected error in configuration: %+v", errs)
		}

		fingerprint, _ := c.configProvider.KeyFingerprint()
		if fingerprint != expected {
			t.Errorf("Expected ConfigProvider.KeyFingerprint: %s, got %s", expected, fingerprint)
		}
	})

	t.Run("instance_defined_tags_json", func(t *testing.T) {
		raw := testConfig(cfgFile)
		raw["instance_defined_tags_json"] = `{ "fo": { "o" : "bar" } }`
		delete(raw, "instance_defined_tags")

		var c Config
		errs := c.Prepare(raw)
		if errs != nil {
			t.Fatalf("Unexpected error in configuration: %+v", errs)
		}

		fo, ok := c.InstanceDefinedTags["fo"]
		if !ok {
			t.Fatalf("unexpected InstanceDefinedTags")
		}
		bar, ok := fo["o"]
		if !ok || bar != "bar" {
			t.Fatalf("unexpected InstanceDefinedTags")
		}
	})

	t.Run("defined_tags_json", func(t *testing.T) {
		raw := testConfig(cfgFile)
		raw["defined_tags_json"] = `{ "fo": { "o" : "bar" } }`
		delete(raw, "defined_tags")

		var c Config
		errs := c.Prepare(raw)
		if errs != nil {
			t.Fatalf("Unexpected error in configuration: %+v", errs)
		}

		fo, ok := c.DefinedTags["fo"]
		if !ok {
			t.Fatalf("unexpected DefinedTags")
		}
		bar, ok := fo["o"]
		if !ok || bar != "bar" {
			t.Fatalf("unexpected DefinedTags")
		}
	})

	t.Run("legacy_imds_endpoints_enabled", func(t *testing.T) {
		raw := testConfig(cfgFile)

		var c Config
		errs := c.Prepare(raw)
		if errs != nil {
			t.Fatalf("Unexpected error in configuration: %+v", errs)
		}

		imdsDisabled := c.InstanceOptions.AreLegacyImdsEndpointsDisabled
		if imdsDisabled != nil && *imdsDisabled {
			t.Errorf("Expected Legacy IMDS to be enabled")
		}
	})

	t.Run("legacy_imds_endpoints_disabled", func(t *testing.T) {
		instanceOptions := map[string]interface{}{
			"are_legacy_imds_endpoints_disabled": true,
		}
		raw := testConfig(cfgFile)
		raw["instance_options"] = instanceOptions

		var c Config
		errs := c.Prepare(raw)
		if errs != nil {
			t.Fatalf("Unexpected error in configuration: %+v", errs)
		}

		imdsDisabled := c.InstanceOptions.AreLegacyImdsEndpointsDisabled
		if imdsDisabled != nil && !*imdsDisabled {
			t.Errorf("Expected Legacy IMDS to be disabled")
		}
	})

	t.Run("create_vnic_details.defined_tags_json", func(t *testing.T) {
		createVNICDetails := map[string]interface{}{
			"defined_tags_json": `{ "fo": { "o" : "bar" } }`,
		}
		raw := testConfig(cfgFile)
		raw["create_vnic_details"] = createVNICDetails
		delete(raw, "defined_tags")

		var c Config
		errs := c.Prepare(raw)
		if errs != nil {
			t.Fatalf("Unexpected error in configuration: %+v", errs)
		}

		fo, ok := c.CreateVnicDetails.DefinedTags["fo"]
		if !ok {
			t.Fatalf("unexpected DefinedTags")
		}
		bar, ok := fo["o"]
		if !ok || bar != "bar" {
			t.Fatalf("unexpected DefinedTags")
		}
	})

	// Test the correct errors are produced when certain template keys
	// are present alongside use_instance_principals key.
	invalidKeys := []string{
		"access_cfg_file",
		"access_cfg_file_account",
		"user_ocid",
		"tenancy_ocid",
		"region",
		"fingerprint",
		"key_file",
		"pass_phrase",
	}
	for _, k := range invalidKeys {
		t.Run(k+"_mixed_with_use_instance_principals", func(t *testing.T) {
			raw := testConfig(cfgFile)
			raw["use_instance_principals"] = "true"
			raw[k] = "some_random_value"

			var c Config

			c.configProvider = instancePrincipalConfigurationProviderMock{}

			errs := c.Prepare(raw)

			if !strings.Contains(errs.Error(), k) {
				t.Errorf("Expected '%s' to contain '%s'", errs.Error(), k)
			}
		})
	}
}

// BaseTestConfig creates the base (DEFAULT) config including a temporary key
// file.
// NOTE: Caller is responsible for removing temporary key file.
func baseTestConfigWithTmpKeyFile() (*ini.File, *os.File, error) {
	keyFile, err := generateRSAKeyFile()
	if err != nil {
		return nil, keyFile, err
	}
	// Build ini
	cfg := ini.Empty()
	section, _ := cfg.NewSection("DEFAULT")
	_, _ = section.NewKey("region", "us-ashburn-1")
	_, _ = section.NewKey("tenancy", "ocid1.tenancy.oc1..aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	_, _ = section.NewKey("user", "ocid1.user.oc1..aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	_, _ = section.NewKey("fingerprint", "70:04:5z:b3:19:ab:90:75:a4:1f:50:d4:c7:c3:33:20")
	_, _ = section.NewKey("key_file", keyFile.Name())

	return cfg, keyFile, nil
}

// WriteTestConfig writes a ini.File to a temporary file for use in unit tests.
// NOTE: Caller is responsible for removing temporary file.
func writeTestConfig(cfg *ini.File) (*os.File, error) {
	confFile, err := ioutil.TempFile("", "config_file")
	if err != nil {
		return nil, err
	}

	if _, err := confFile.Write([]byte("[DEFAULT]\n")); err != nil {
		os.Remove(confFile.Name())
		return nil, err
	}

	if _, err := cfg.WriteTo(confFile); err != nil {
		os.Remove(confFile.Name())
		return nil, err
	}
	return confFile, nil
}

// generateRSAKeyFile generates an RSA key file for use in unit tests.
// NOTE: The caller is responsible for deleting the temporary file.
func generateRSAKeyFile() (*os.File, error) {
	// Create temporary file for the key
	f, err := ioutil.TempFile("", "key")
	if err != nil {
		return nil, err
	}

	// Generate key
	priv, err := rsa.GenerateKey(rand.Reader, 2014)
	if err != nil {
		return nil, err
	}

	// ASN.1 DER encoded form
	privDer := x509.MarshalPKCS1PrivateKey(priv)
	privBlk := pem.Block{
		Type:    "RSA PRIVATE KEY",
		Headers: nil,
		Bytes:   privDer,
	}

	// Write the key out
	if _, err := f.Write(pem.EncodeToMemory(&privBlk)); err != nil {
		return nil, err
	}

	return f, nil
}
