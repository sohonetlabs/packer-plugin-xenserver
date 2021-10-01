package common

import (
	"context"
	"fmt"
	"github.com/xenserver/packer-builder-xenserver/builder/xenserver/common/xen"
	"log"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/packer"
	xenapi "github.com/terra-farm/go-xen-api-client"
)

type StepUploadVdi struct {
	VdiNameFunc   func() string
	ImagePathFunc func() string
	VdiUuidKey    string
}

func (self *StepUploadVdi) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	config := state.Get("commonconfig").(CommonConfig)
	ui := state.Get("ui").(packer.Ui)
	c := state.Get("client").(*xen.Connection)

	imagePath := self.ImagePathFunc()
	vdiName := self.VdiNameFunc()
	if imagePath == "" {
		// skip if no disk image to attach
		return multistep.ActionContinue
	}

	ui.Say(fmt.Sprintf("Step: Upload VDI '%s'", vdiName))

	// Create VDI for the image
	srs, err := c.GetClient().SR.GetAll(c.GetSessionRef())
	ui.Say(fmt.Sprintf("Step: Found SRs '%v'", srs))

	sr, err := config.GetISOSR(c)

	if err != nil {
		ui.Error(fmt.Sprintf("Unable to get SR: %v", err))
		return multistep.ActionHalt
	}

	// Open the file for reading (NB: HTTPUpload closes the file for us)
	fh, err := os.Open(imagePath)
	if err != nil {
		ui.Error(fmt.Sprintf("Unable to open disk image '%s': %s", imagePath, err.Error()))
		return multistep.ActionHalt
	}

	// Get file length
	fstat, err := fh.Stat()
	if err != nil {
		ui.Error(fmt.Sprintf("Unable to stat disk image '%s': %s", imagePath, err.Error()))
		return multistep.ActionHalt
	}
	fileLength := fstat.Size()

	// Create the VDI
	// vdi, err := sr.CreateVdi(vdiName, fileLength)
	vdi, err := c.GetClient().VDI.Create(c.GetSessionRef(), xenapi.VDIRecord{
		NameLabel:   vdiName,
		VirtualSize: int(fileLength),
		Type:        "user",
		Sharable:    false,
		ReadOnly:    false,
		SR:          sr,
		OtherConfig: map[string]string{
			"temp": "temp",
		},
	})
	if err != nil {
		ui.Error(fmt.Sprintf("Unable to create VDI '%s': %s", vdiName, err.Error()))
		return multistep.ActionHalt
	}

	vdiUuid, err := c.GetClient().VDI.GetUUID(c.GetSessionRef(), vdi)
	if err != nil {
		ui.Error(fmt.Sprintf("Unable to get UUID of VDI '%s': %s", vdiName, err.Error()))
		return multistep.ActionHalt
	}
	state.Put(self.VdiUuidKey, vdiUuid)

	_, err = HTTPUpload(fmt.Sprintf("https://%s/import_raw_vdi?vdi=%s&session_id=%s",
		net.JoinHostPort(c.Host, strconv.Itoa(c.Port)),
		vdi,
		c.GetSession(),
	), fh, state)
	if err != nil {
		ui.Error(fmt.Sprintf("Unable to upload VDI: %s", err.Error()))
		return multistep.ActionHalt
	}

	return multistep.ActionContinue
}

func (self *StepUploadVdi) Cleanup(state multistep.StateBag) {
	config := state.Get("commonconfig").(CommonConfig)
	ui := state.Get("ui").(packer.Ui)
	c := state.Get("client").(*xen.Connection)

	vdiName := self.VdiNameFunc()

	if config.ShouldKeepVM(state) {
		return
	}

	vdiUuidRaw, ok := state.GetOk(self.VdiUuidKey)
	if !ok {
		// VDI doesn't exist
		return
	}

	vdiUuid := vdiUuidRaw.(string)
	if vdiUuid == "" {
		// VDI already cleaned up
		return
	}

	vdi, err := c.GetClient().VDI.GetByUUID(c.GetSessionRef(), vdiUuid)
	if err != nil {
		ui.Error(fmt.Sprintf("Can't get VDI '%s': %s", vdiUuid, err.Error()))
		return
	}

	// an interrupted import_raw_vdi takes a while to release the VDI
	// so try several times
	for i := 0; i < 3; i++ {
		log.Printf("Trying to destroy VDI...")
		err = c.GetClient().VDI.Destroy(c.GetSessionRef(), vdi)
		if err == nil {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if err != nil {
		ui.Error(fmt.Sprintf("Can't destroy VDI '%s': %s", vdiUuid, err.Error()))
		return
	}
	ui.Say(fmt.Sprintf("Destroyed VDI '%s'", vdiName))

	state.Put(self.VdiUuidKey, "")
}
