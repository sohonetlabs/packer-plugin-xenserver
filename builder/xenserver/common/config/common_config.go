package config

import (
	"errors"
	"fmt"
	"github.com/hashicorp/packer-plugin-sdk/bootcommand"
	"github.com/hashicorp/packer-plugin-sdk/common"
	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/multistep/commonsteps"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/shutdowncommand"
	hconfig "github.com/hashicorp/packer-plugin-sdk/template/config"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"
	xenapi "github.com/terra-farm/go-xen-api-client"
	"github.com/xenserver/packer-builder-xenserver/builder/xenserver/common/xen"
)

type CommonConfig struct {
	common.PackerConfig    `mapstructure:",squash"`
	bootcommand.VNCConfig  `mapstructure:",squash"`
	commonsteps.HTTPConfig `mapstructure:",squash"`

	XenConfig `mapstructure:",squash"`

	VMName             string   `mapstructure:"vm_name"`
	VMDescription      string   `mapstructure:"vm_description"`
	SrName             string   `mapstructure:"sr_name"`
	SrISOName          string   `mapstructure:"sr_iso_name"`
	FloppyFiles        []string `mapstructure:"floppy_files"`
	NetworkNames       []string `mapstructure:"network_names"`
	ExportNetworkNames []string `mapstructure:"export_network_names"`

	PlatformArgs map[string]string `mapstructure:"platform_args"`

	shutdowncommand.ShutdownConfig `mapstructure:",squash"`

	ToolsIsoName string `mapstructure:"tools_iso_name"`

	CommConfig `mapstructure:",squash"`

	OutputDir string `mapstructure:"output_directory"`
	Format    string `mapstructure:"format"`
	KeepVM    string `mapstructure:"keep_vm"`
	IPGetter  string `mapstructure:"ip_getter"`

	Firmware string `mapstructure:"firmware"`
	HardwareConfig

	ctx interpolate.Context
}

func (c *CommonConfig) GetInterpContext() *interpolate.Context {
	return &c.ctx
}

func (c *CommonConfig) Prepare(upper interface{}, raws ...interface{}) ([]string, []string, error) {

	err := hconfig.Decode(upper, &hconfig.DecodeOpts{
		Interpolate: true,
		InterpolateFilter: &interpolate.RenderFilter{
			Exclude: []string{
				"boot_command",
			},
		},
	}, raws...)

	if err != nil {
		return nil, nil, err
	}

	var errs *packersdk.MultiError
	var warnings []string

	// Set default values

	if c.Firmware == "" {
		c.Firmware = "bios"
	}

	if c.ToolsIsoName == "" {
		c.ToolsIsoName = "xs-tools.iso"
	}

	if c.HTTPPortMin == 0 {
		c.HTTPPortMin = 8000
	}

	if c.HTTPPortMax == 0 {
		c.HTTPPortMax = 9000
	}

	if c.FloppyFiles == nil {
		c.FloppyFiles = make([]string, 0)
	}

	if c.OutputDir == "" {
		c.OutputDir = fmt.Sprintf("output-%s", c.PackerConfig.PackerBuildName)
	}

	if c.VMName == "" {
		c.VMName = fmt.Sprintf("packer-%s-{{timestamp}}", c.PackerConfig.PackerBuildName)
	}

	if c.Format == "" {
		c.Format = "xva"
	}

	if c.KeepVM == "" {
		c.KeepVM = "never"
	}

	if c.IPGetter == "" {
		c.IPGetter = "auto"
	}

	if len(c.PlatformArgs) == 0 {
		pargs := make(map[string]string)
		pargs["viridian"] = "false"
		pargs["nx"] = "true"
		pargs["pae"] = "true"
		pargs["apic"] = "true"
		pargs["timeoffset"] = "0"
		pargs["acpi"] = "1"
		c.PlatformArgs = pargs
	}

	// Validation

	if c.HTTPPortMin > c.HTTPPortMax {
		errs = packersdk.MultiErrorAppend(errs, errors.New("the HTTP min port must be less than the max"))
	}

	switch c.Format {
	case "xva", "xva_compressed", "vdi_raw", "vdi_vhd", "none":
	default:
		errs = packersdk.MultiErrorAppend(errs, errors.New("format must be one of 'xva', 'vdi_raw', 'vdi_vhd', 'none'"))
	}

	switch c.KeepVM {
	case "always", "never", "on_success":
	default:
		errs = packersdk.MultiErrorAppend(errs, errors.New("keep_vm must be one of 'always', 'never', 'on_success'"))
	}

	switch c.IPGetter {
	case "auto", "tools", "http":
	default:
		errs = packersdk.MultiErrorAppend(errs, errors.New("ip_getter must be one of 'auto', 'tools', 'http'"))
	}

	innerWarnings, es := c.CommConfig.Prepare(&c.ctx)
	errs = packersdk.MultiErrorAppend(errs, es...)
	warnings = append(warnings, innerWarnings...)

	innerWarnings, es = c.XenConfig.Prepare(&c.ctx)
	errs = packersdk.MultiErrorAppend(errs, es...)
	warnings = append(warnings, innerWarnings...)

	innerWarnings, es = c.HardwareConfig.Prepare(&c.ctx)
	errs = packersdk.MultiErrorAppend(errs, es...)
	warnings = append(warnings, innerWarnings...)

	errs = packersdk.MultiErrorAppend(errs, c.VNCConfig.Prepare(&c.ctx)...)
	errs = packersdk.MultiErrorAppend(errs, c.HTTPConfig.Prepare(&c.ctx)...)
	errs = packersdk.MultiErrorAppend(errs, c.ShutdownConfig.Prepare(&c.ctx)...)

	return nil, warnings, errs
}

// steps should check config.ShouldKeepVM first before cleaning up the VM
func (c CommonConfig) ShouldKeepVM(state multistep.StateBag) bool {
	switch c.KeepVM {
	case "always":
		return true
	case "never":
		return false
	case "on_success":
		// only keep instance if build was successful
		_, cancelled := state.GetOk(multistep.StateCancelled)
		_, halted := state.GetOk(multistep.StateHalted)
		return !(cancelled || halted)
	default:
		panic(fmt.Sprintf("Unknown keep_vm value '%s'", c.KeepVM))
	}
}

func (config CommonConfig) GetSR(c *xen.Connection) (xenapi.SRRef, error) {
	var srRef xenapi.SRRef
	if config.SrName == "" {
		hostRef, err := c.GetClient().Session.GetThisHost(c.GetSessionRef(), c.GetSessionRef())

		if err != nil {
			return srRef, err
		}

		pools, err := c.GetClient().Pool.GetAllRecords(c.GetSessionRef())

		if err != nil {
			return srRef, err
		}

		for _, pool := range pools {
			if pool.Master == hostRef {
				return pool.DefaultSR, nil
			}
		}

		return srRef, errors.New(fmt.Sprintf("failed to find default SR on host '%s'", hostRef))

	} else {
		// Use the provided name label to find the SR to use
		srs, err := c.GetClient().SR.GetByNameLabel(c.GetSessionRef(), config.SrName)

		if err != nil {
			return srRef, err
		}

		switch {
		case len(srs) == 0:
			return srRef, fmt.Errorf("Couldn't find a SR with the specified name-label '%s'", config.SrName)
		case len(srs) > 1:
			return srRef, fmt.Errorf("Found more than one SR with the name '%s'. The name must be unique", config.SrName)
		}

		return srs[0], nil
	}
}

func (config CommonConfig) GetISOSR(c *xen.Connection) (xenapi.SRRef, error) {
	var srRef xenapi.SRRef
	if config.SrISOName == "" {
		return srRef, errors.New("sr_iso_name must be specified in the packer configuration")

	} else {
		// Use the provided name label to find the SR to use
		srs, err := c.GetClient().SR.GetByNameLabel(c.GetSessionRef(), config.SrName)

		if err != nil {
			return srRef, err
		}

		switch {
		case len(srs) == 0:
			return srRef, fmt.Errorf("Couldn't find a SR with the specified name-label '%s'", config.SrName)
		case len(srs) > 1:
			return srRef, fmt.Errorf("Found more than one SR with the name '%s'. The name must be unique", config.SrName)
		}

		return srs[0], nil
	}
}