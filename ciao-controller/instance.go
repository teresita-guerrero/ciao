/*
// Copyright (c) 2016 Intel Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
*/

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/ciao-project/ciao/ciao-controller/api"
	"github.com/ciao-project/ciao/ciao-controller/types"
	"github.com/ciao-project/ciao/ciao-controller/utils"
	"github.com/ciao-project/ciao/payloads"
	"github.com/ciao-project/ciao/uuid"
	"github.com/golang/glog"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"
)

type config struct {
	sc     payloads.Start
	config string
	cnci   bool
	mac    string
	ip     string
}

type instance struct {
	*types.Instance
	newConfig config
	ctl       *controller
	startTime time.Time
}

type userData struct {
	UUID     string `json:"uuid"`
	Hostname string `json:"hostname"`
}

func isCNCIWorkload(workload *types.Workload) bool {
	return workload.Requirements.NetworkNode
}

func newInstance(ctl *controller, tenantID string, workload *types.Workload,
	name string, subnet string, IPAddr net.IP) (*instance, error) {
	id := uuid.Generate()

	if name != "" {
		existingID, err := ctl.ds.ResolveInstance(tenantID, name)
		if err != nil {
			return nil, errors.Wrap(err, "error trying to resolve name")
		}

		if existingID != "" {
			return nil, fmt.Errorf("Instance name already in use: %s", name)
		}
	}

	config, err := newConfig(ctl, workload, id.String(), tenantID, name, IPAddr)
	if err != nil {
		return nil, err
	}

	newInstance := types.Instance{
		TenantID:    tenantID,
		WorkloadID:  workload.ID,
		State:       payloads.Pending,
		ID:          id.String(),
		CNCI:        config.cnci,
		IPAddress:   config.ip,
		VnicUUID:    config.sc.Start.Networking.VnicUUID,
		Subnet:      config.sc.Start.Networking.Subnet,
		MACAddress:  config.mac,
		CreateTime:  time.Now(),
		Name:        name,
		StateChange: sync.NewCond(&sync.Mutex{}),
	}

	if subnet != "" {
		newInstance.Subnet = subnet
	}

	i := &instance{
		ctl:       ctl,
		newConfig: config,
		Instance:  &newInstance,
	}

	return i, nil
}

func (i *instance) Add() error {
	ds := i.ctl.ds
	var err error
	err = ds.AddInstance(i.Instance)
	if err != nil {
		return errors.Wrapf(err, "Error creating instance in datastore")
	}

	for _, volume := range i.newConfig.sc.Start.Storage {
		if volume.ID == "" && volume.Local {
			// these are launcher auto-created ephemeral
			continue
		}
		_, err = ds.GetBlockDevice(volume.ID)
		if err != nil {
			return fmt.Errorf("Invalid block device mapping.  %s already in use", volume.ID)
		}

		_, err = ds.CreateStorageAttachment(i.Instance.ID, volume)
		if err != nil {
			return errors.Wrap(err, "Error creating storage attachment")
		}
	}

	return nil
}

func (i *instance) Clean() error {
	if i.CNCI {
		// CNCI resources are not tracked by quota system
		return nil
	}

	err := i.ctl.ds.ReleaseTenantIP(i.TenantID, i.IPAddress)
	if err != nil {
		return errors.Wrap(err, "error releasing tenant IP")
	}

	wl, err := i.ctl.ds.GetWorkload(i.WorkloadID)
	if err != nil {
		return errors.Wrap(err, "error getting workload from datastore")
	}

	resources := []payloads.RequestedResource{
		{Type: payloads.Instance, Value: 1},
		{Type: payloads.MemMB, Value: wl.Requirements.MemMB},
		{Type: payloads.VCPUs, Value: wl.Requirements.VCPUs}}
	i.ctl.qs.Release(i.TenantID, resources...)

	err = i.ctl.deleteEphemeralStorage(i.ID)
	if err != nil {
		return errors.Wrap(err, "error deleting ephemeral strorage")
	}

	return nil
}

func (i *instance) Allowed() (bool, error) {
	if i.CNCI == true {
		// should I bother to check the tenant id exists?
		return true, nil
	}

	ds := i.ctl.ds

	wl, err := ds.GetWorkload(i.WorkloadID)
	if err != nil {
		return true, errors.Wrap(err, "error getting workload from datastore")
	}

	resources := []payloads.RequestedResource{
		{Type: payloads.Instance, Value: 1},
		{Type: payloads.MemMB, Value: wl.Requirements.MemMB},
		{Type: payloads.VCPUs, Value: wl.Requirements.VCPUs}}
	res := <-i.ctl.qs.Consume(i.TenantID, resources...)

	// Cleanup on disallowed happens in Clean()
	return res.Allowed(), nil
}

func instanceActive(i *types.Instance) bool {
	i.StateLock.RLock()
	defer i.StateLock.RUnlock()

	if i.State == payloads.Running {
		return true
	}

	return false
}

func getStorage(c *controller, s types.StorageResource, tenant string, instanceID string) (payloads.StorageResource, error) {
	// storage already exists, use preexisting definition.
	if s.ID != "" {
		return payloads.StorageResource{ID: s.ID, Bootable: s.Bootable}, nil
	}

	var err error
	req := api.RequestedVolume{
		Description: fmt.Sprintf("Volume for instance: %s", instanceID),
		Internal:    s.Internal,
		Size:        s.Size,
	}

	switch s.SourceType {
	case types.ImageService:
		req.ImageRef = s.Source
	case types.VolumeService:
		req.SourceVolID = s.Source
	case types.Empty:
		break
	default:
		return payloads.StorageResource{}, errors.New("Unsupported workload storage variant in getStorage()")
	}

	volume, err := c.CreateVolume(tenant, req)
	if err != nil {
		return payloads.StorageResource{}, errors.Wrap(err, "Error creating volume")
	}
	return payloads.StorageResource{ID: volume.ID, Bootable: s.Bootable, Ephemeral: s.Ephemeral}, nil
}

func networkConfig(ctl *controller, tenant *types.Tenant, networking *payloads.NetworkResources, cnci bool, ipAddress net.IP) error {
	networking.VnicUUID = uuid.Generate().String()

	if cnci {
		hwaddr, err := utils.NewHardwareAddr()
		if err != nil {
			return err
		}

		networking.VnicMAC = hwaddr.String()
		return nil
	}

	networking.VnicMAC = utils.NewTenantHardwareAddr(ipAddress).String()

	// send in CIDR notation?
	networking.PrivateIP = ipAddress.String()
	mask := net.CIDRMask(tenant.SubnetBits, 32)
	ipnet := net.IPNet{
		IP:   ipAddress.Mask(mask),
		Mask: mask,
	}
	networking.Subnet = ipnet.String()

	cnciInstance, err := tenant.CNCIctrl.GetSubnetCNCI(networking.Subnet)
	if err != nil {
		return err
	}

	networking.ConcentratorUUID = cnciInstance.ID

	// in theory we should refuse to go on if ip is null
	// for now let's keep going
	networking.ConcentratorIP = cnciInstance.IPAddress
	return nil
}

func newConfig(ctl *controller, wl *types.Workload, instanceID string, tenantID string,
	name string, IPaddr net.IP) (config, error) {
	var metaData userData
	var config config
	var networking payloads.NetworkResources
	var storage []payloads.StorageResource

	baseConfig := wl.Config

	fwType := wl.FWType
	config.cnci = isCNCIWorkload(wl)
	metaData.UUID = instanceID

	tenant, err := ctl.ds.GetTenant(tenantID)
	if err != nil {
		fmt.Println("unable to get tenant")
	}

	err = networkConfig(ctl, tenant, &networking, config.cnci, IPaddr)
	if err != nil {
		return config, err
	}

	metaData.Hostname = instanceID
	if name != "" {
		metaData.Hostname = name
	}

	config.ip = networking.PrivateIP

	// handle storage resources in workload definition
	for i := range wl.Storage {
		workloadStorage, err := getStorage(ctl, wl.Storage[i], tenantID, instanceID)
		if err != nil {
			return config, err
		}
		storage = append(storage, workloadStorage)
	}

	// hardcode persistence until changes can be made to workload
	// template datastore.  Estimated resources can be blank
	// for now because we don't support it yet.
	startCmd := payloads.StartCmd{
		TenantUUID:          tenantID,
		InstanceUUID:        instanceID,
		FWType:              payloads.Firmware(fwType),
		VMType:              wl.VMType,
		InstancePersistence: payloads.Host,
		Networking:          networking,
		Storage:             storage,
		Requirements:        wl.Requirements,
	}

	if wl.VMType == payloads.Docker {
		startCmd.DockerImage = wl.ImageName
	}

	cmd := payloads.Start{
		Start: startCmd,
	}
	config.sc = cmd

	y, err := yaml.Marshal(&config.sc)
	if err != nil {
		glog.Warning("error marshalling config: ", err)
	}

	b, err := json.MarshalIndent(metaData, "", "\t")
	if err != nil {
		glog.Warning("error marshalling user data: ", err)
	}

	config.config = "---\n" + string(y) + "...\n" + baseConfig + "---\n" + string(b) + "\n...\n"
	config.mac = networking.VnicMAC

	return config, err
}
