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
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/ciao-project/ciao/ciao-controller/api"
	"github.com/ciao-project/ciao/ciao-controller/internal/datastore"
	"github.com/ciao-project/ciao/ciao-controller/internal/quotas"
	storage "github.com/ciao-project/ciao/ciao-storage"
	"github.com/ciao-project/ciao/clogger/gloginterface"
	"github.com/ciao-project/ciao/database"
	"github.com/ciao-project/ciao/osprepare"
	"github.com/ciao-project/ciao/ssntp"
	"github.com/golang/glog"
	"github.com/pkg/errors"
)

type tenantConfirmMemo struct {
	ch  chan struct{}
	err error
}

type controller struct {
	storage.BlockDriver
	client              controllerClient
	ds                  *datastore.Datastore
	apiURL              string
	tenantReadiness     map[string]*tenantConfirmMemo
	tenantReadinessLock sync.Mutex
	qs                  *quotas.Quotas
	httpServers         []*http.Server
}

type cnciNetFlag string

func (c *cnciNetFlag) String() string {
	return string(*c)
}

func (c *cnciNetFlag) Set(val string) error {
	IP := net.ParseIP(val)
	if IP == nil {
		return fmt.Errorf("Unable to parse CNCI network address")
	}

	*c = cnciNetFlag(IP.String())

	return nil
}

var cert = flag.String("cert", "", "Client certificate")
var caCert = flag.String("cacert", "", "CA certificate")
var serverURL = flag.String("url", "", "Server URL")
var prepare = flag.Bool("osprepare", false, "Install dependencies")
var controllerAPIPort = api.Port
var httpsCAcert = "/etc/pki/ciao/ciao-controller-cacert.pem"
var httpsKey = "/etc/pki/ciao/ciao-controller-key.pem"
var workloadsPath = flag.String("workloads_path", "/var/lib/ciao/data/controller/workloads", "path to yaml files")
var persistentDatastoreLocation = flag.String("database_path", "/var/lib/ciao/data/controller/ciao-controller.db", "path to persistent database")
var logDir = "/var/lib/ciao/logs/controller"

var clientCertCAPath = "/etc/pki/ciao/auth-CA.pem"

var cephID = flag.String("ceph_id", "", "ceph client id")

var adminSSHKey = ""

// this default allows us to have up to 32K hosts within the upper part
// of the 192.168.0.0/16 private address space.
var cnciNet cnciNetFlag = "192.168.128.0"

func init() {
	flag.Parse()

	if *prepare {
		logToStderr := flag.Lookup("logtostderr")
		if logToStderr != nil {
			logToStderr.Value.Set("true")
		}
		return
	}

	logDirFlag := flag.Lookup("log_dir")
	if logDirFlag == nil {
		glog.Errorf("log_dir does not exist")
		return
	}

	if logDirFlag.Value.String() == "" {
		if err := logDirFlag.Value.Set(logDir); err != nil {
			glog.Errorf("Error setting log directory: %v", err)
			return
		}
	}

	if err := os.MkdirAll(logDirFlag.Value.String(), 0755); err != nil {
		glog.Errorf("Unable to create log directory (%s) %v", logDir, err)
		return
	}
}

func getNameFromCert(httpsCAcert, httpsKey string) (string, error) {
	cert, err := tls.LoadX509KeyPair(httpsCAcert, httpsKey)
	if err != nil {
		return "", errors.Wrap(err, "Error loading certificate pair")
	}

	// leaf is first
	if len(cert.Certificate) == 0 {
		return "", errors.New("Expected at least one certificate in encoded data")
	}

	c, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return "", errors.Wrap(err, "Error parsing certificate")
	}

	glog.Infof("Got name from certificate: %s", c.Subject.CommonName)
	return c.Subject.CommonName, nil
}

func main() {
	if *prepare {
		logger := gloginterface.CiaoGlogLogger{}
		osprepare.Bootstrap(context.TODO(), logger)
		osprepare.InstallDeps(context.TODO(), controllerDeps, logger)
		return
	}

	var wg sync.WaitGroup
	var err error

	ctl := new(controller)
	ctl.tenantReadiness = make(map[string]*tenantConfirmMemo)
	ctl.ds = new(datastore.Datastore)
	ctl.qs = new(quotas.Quotas)

	dsConfig := datastore.Config{
		PersistentURI:     "file:" + *persistentDatastoreLocation,
		InitWorkloadsPath: *workloadsPath,
	}

	err = ctl.ds.Init(dsConfig)
	if err != nil {
		glog.Fatalf("unable to Init datastore: %s", err)
		return
	}

	ctl.qs.Init()
	err = populateQuotasFromDatastore(ctl.qs, ctl.ds)
	if err != nil {
		glog.Fatalf("Error populating quotas from datastore: %v", err)
		return
	}

	config := &ssntp.Config{
		URI:    *serverURL,
		CAcert: *caCert,
		Cert:   *cert,
		Log:    ssntp.Log,
	}

	ctl.client, err = newSSNTPClient(ctl, config)
	if err != nil {
		// spawn some retry routine?
		glog.Fatalf("unable to connect to SSNTP server")
		return
	}

	ssntpClient := ctl.client.ssntpClient()
	clusterConfig, err := ssntpClient.ClusterConfiguration()
	if err != nil {
		glog.Fatalf("Unable to retrieve Cluster Configuration: %v", err)
		return
	}

	controllerAPIPort = clusterConfig.Configure.Controller.CiaoPort
	httpsCAcert = clusterConfig.Configure.Controller.HTTPSCACert
	httpsKey = clusterConfig.Configure.Controller.HTTPSKey
	if *cephID == "" {
		*cephID = clusterConfig.Configure.Storage.CephID
	}

	cnciVCPUs := clusterConfig.Configure.Controller.CNCIVcpus
	cnciMem := clusterConfig.Configure.Controller.CNCIMem
	cnciDisk := clusterConfig.Configure.Controller.CNCIDisk

	adminSSHKey = clusterConfig.Configure.Controller.AdminSSHKey

	if clusterConfig.Configure.Controller.ClientAuthCACertPath != "" {
		clientCertCAPath = clusterConfig.Configure.Controller.ClientAuthCACertPath
	}

	if clusterConfig.Configure.Controller.CNCINet != "" {
		err = cnciNet.Set(clusterConfig.Configure.Controller.CNCINet)
		if err != nil {
			glog.Fatalf("Invalid CNCI Net cluster configuration: %v", err)
			return
		}
	}

	ctl.ds.GenerateCNCIWorkload(cnciVCPUs, cnciMem, cnciDisk, adminSSHKey)

	database.Logger = gloginterface.CiaoGlogLogger{}

	ctl.BlockDriver = func() storage.BlockDriver {
		driver := storage.CephDriver{
			ID: *cephID,
		}
		return driver
	}()

	err = initializeCNCICtrls(ctl)
	if err != nil {
		glog.Fatal("Unable to initialize CNCI controllers: ", err)
		return
	}

	host, err := getNameFromCert(httpsCAcert, httpsKey)
	if err != nil {
		glog.Warningf("Unable to get name from certificate: %s", err)
		host, _ = os.Hostname()
	}

	ctl.apiURL = fmt.Sprintf("https://%s:%d", host, controllerAPIPort)

	server, err := ctl.createCiaoServer()
	if err != nil {
		glog.Fatalf("Error creating ciao server: %v", err)
	}
	ctl.httpServers = append(ctl.httpServers, server)

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-signalCh
		glog.Warningf("Received signal: %s", s)
		ctl.ShutdownHTTPServers()
		shutdownCNCICtrls(ctl)
	}()

	for _, server := range ctl.httpServers {
		wg.Add(1)
		go func(server *http.Server) {
			if err := server.ListenAndServeTLS(httpsCAcert, httpsKey); err != http.ErrServerClosed {
				glog.Errorf("Error from HTTP server: %v", err)
			}
			wg.Done()
		}(server)
	}

	wg.Wait()
	glog.Warning("Controller shutdown initiated")
	ctl.qs.Shutdown()
	ctl.ds.Exit()
	ctl.client.Disconnect()
	glog.Flush()
}
