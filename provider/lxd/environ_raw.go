// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

// +build go1.3

package lxd

import (
	"github.com/juju/errors"
	lxdclient "github.com/lxc/lxd/client"
	lxdapi "github.com/lxc/lxd/shared/api"

	"github.com/juju/juju/container/lxd"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/network"
	jujulxdclient "github.com/juju/juju/tools/lxdclient"
)

// TODO (manadart 2018-05-09) This is really nothing but an LXD client and does
// not need its own type.
// As the LXD facility is refactored, this will be removed altogether.
// As an interim measure, the new and old client implementations will be have
// interface shims.
// After the old client is removed, provider tests can be rewritten using
// GoMock, at which point rawProvider is replaced with the new client.
type rawProvider struct {
	newClient
	lxdCerts
	lxdInstances
	lxdProfiles
	lxdStorage

	remote jujulxdclient.Remote
}

type newClient interface {
	FindImage(string, string, []lxd.RemoteServer, bool, environs.StatusCallbackFunc) (lxd.SourcedImage, error)
	GetServer() (server *lxdapi.Server, ETag string, err error)
	GetConnectionInfo() (info *lxdclient.ConnectionInfo, err error)
	UpdateServerConfig(map[string]interface{}) error
	UpdateContainerConfig(string, map[string]string) error
}

type lxdCerts interface {
	AddCert(jujulxdclient.Cert) error
	CertByFingerprint(string) (lxdapi.Certificate, error)
	RemoveCertByFingerprint(string) error
}

type lxdInstances interface {
	Instances(string, ...string) ([]jujulxdclient.Instance, error)
	AddInstance(jujulxdclient.InstanceSpec) (*jujulxdclient.Instance, error)
	RemoveInstances(string, ...string) error
	Addresses(string) ([]network.Address, error)
	AttachDisk(string, string, jujulxdclient.DiskDevice) error
	RemoveDevice(string, string) error
}

type lxdProfiles interface {
	DefaultProfileBridgeName() string
	CreateProfile(string, map[string]string) error
	HasProfile(string) (bool, error)
}

type lxdStorage interface {
	StorageSupported() bool

	StoragePool(name string) (lxdapi.StoragePool, error)
	StoragePools() ([]lxdapi.StoragePool, error)
	CreateStoragePool(name, driver string, attrs map[string]string) error

	Volume(pool, volume string) (lxdapi.StorageVolume, error)
	VolumeCreate(pool, volume string, config map[string]string) error
	VolumeDelete(pool, volume string) error
	VolumeUpdate(pool, volume string, update lxdapi.StorageVolume) error
	VolumeList(pool string) ([]lxdapi.StorageVolume, error)
}

func newRawProvider(spec environs.CloudSpec, local bool) (*rawProvider, error) {
	if local {
		return newLocalRawProvider()
	}
	return newRemoteRawProvider(spec)
}

func newLocalRawProvider() (*rawProvider, error) {
	config := jujulxdclient.Config{Remote: jujulxdclient.Local}
	return newRawProviderFromConfig(config)
}

func newRemoteRawProvider(spec environs.CloudSpec) (*rawProvider, error) {
	config, err := getRemoteConfig(spec)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return newRawProviderFromConfig(*config)
}

func newRawProviderFromConfig(config jujulxdclient.Config) (*rawProvider, error) {
	client, err := jujulxdclient.Connect(config, true)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return &rawProvider{
		newClient:    client,
		lxdCerts:     client,
		lxdInstances: client,
		lxdProfiles:  client,
		lxdStorage:   client,
		remote:       config.Remote,
	}, nil
}

// getRemoteConfig returns a jujulxdclient.Config using a TCP-based remote.
func getRemoteConfig(spec environs.CloudSpec) (*jujulxdclient.Config, error) {
	clientCert, serverCert, ok := getCerts(spec)
	if !ok {
		return nil, errors.NotValidf("credentials")
	}
	return &jujulxdclient.Config{
		jujulxdclient.Remote{
			Name:          "remote",
			Host:          spec.Endpoint,
			Protocol:      jujulxdclient.LXDProtocol,
			Cert:          clientCert,
			ServerPEMCert: serverCert,
		},
	}, nil
}

func getCerts(spec environs.CloudSpec) (client *jujulxdclient.Cert, server string, ok bool) {
	if spec.Credential == nil {
		return nil, "", false
	}
	credAttrs := spec.Credential.Attributes()
	clientCertPEM, ok := credAttrs[credAttrClientCert]
	if !ok {
		return nil, "", false
	}
	clientKeyPEM, ok := credAttrs[credAttrClientKey]
	if !ok {
		return nil, "", false
	}
	serverCertPEM, ok := credAttrs[credAttrServerCert]
	if !ok {
		return nil, "", false
	}
	clientCert := &jujulxdclient.Cert{
		Name:    "juju",
		CertPEM: []byte(clientCertPEM),
		KeyPEM:  []byte(clientKeyPEM),
	}
	return clientCert, serverCertPEM, true
}
