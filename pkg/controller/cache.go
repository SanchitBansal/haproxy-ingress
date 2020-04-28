/*
Copyright 2019 The HAProxy Ingress Controller Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	api "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	"github.com/jcmoraisjr/haproxy-ingress/pkg/acme"
	cfile "github.com/jcmoraisjr/haproxy-ingress/pkg/common/file"
	"github.com/jcmoraisjr/haproxy-ingress/pkg/common/ingress/controller"
	"github.com/jcmoraisjr/haproxy-ingress/pkg/common/net/ssl"
	convtypes "github.com/jcmoraisjr/haproxy-ingress/pkg/converters/types"
	"github.com/jcmoraisjr/haproxy-ingress/pkg/types"
	"github.com/jcmoraisjr/haproxy-ingress/pkg/utils"
)

const dhparamFilename = "dhparam.pem"

type k8scache struct {
	client                 k8s.Interface
	listers                *listers
	controller             *controller.GenericController
	crossNS                bool
	globalConfigMapKey     string
	tcpConfigMapKey        string
	acmeSecretKeyName      string
	acmeTokenConfigmapName string
	//
	updateQueue utils.Queue
	stateMutex  sync.RWMutex
	clear       bool
	needResync  bool
	//
	globalConfigMapData    map[string]string
	tcpConfigMapData       map[string]string
	newGlobalConfigMapData map[string]string
	newTCPConfigMapData    map[string]string
	//
	delIngresses []*extensions.Ingress
	updIngresses []*extensions.Ingress
	addIngresses []*extensions.Ingress
	newEndpoints []*api.Endpoints
	delServices  []*api.Service
	updServices  []*api.Service
	addServices  []*api.Service
	delSecrets   []*api.Secret
	updSecrets   []*api.Secret
	addSecrets   []*api.Secret
	newPods      []*api.Pod
	//
}

func createCache(
	logger types.Logger,
	client k8s.Interface,
	controller *controller.GenericController,
	updateQueue utils.Queue,
	watchNamespace string,
	resync time.Duration,
) *k8scache {
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		// TODO implement a smart fallback or error checking
		// Fallback to a valid name if envvar is not provided. Should never be used because:
		// - `namespace` is only used in `acme*`
		// - `acme*` is only used by acme client and server
		// - acme client and server are only used if leader elector is enabled
		// - leader elector will panic if this envvar is not provided
		namespace = "default"
	}
	cfg := controller.GetConfig()
	acmeSecretKeyName := cfg.AcmeSecretKeyName
	if !strings.Contains(acmeSecretKeyName, "/") {
		acmeSecretKeyName = namespace + "/" + acmeSecretKeyName
	}
	acmeTokenConfigmapName := cfg.AcmeTokenConfigmapName
	if !strings.Contains(acmeTokenConfigmapName, "/") {
		acmeTokenConfigmapName = namespace + "/" + acmeTokenConfigmapName
	}
	globalConfigMapName := cfg.ConfigMapName
	tcpConfigMapName := cfg.TCPConfigMapName
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(logger.Info)
	eventBroadcaster.StartRecordingToSink(&typedv1.EventSinkImpl{
		Interface: client.CoreV1().Events(watchNamespace),
	})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, api.EventSource{
		Component: "ingress-controller",
	})
	cache := &k8scache{
		client:                 client,
		controller:             controller,
		crossNS:                cfg.AllowCrossNamespace,
		globalConfigMapKey:     globalConfigMapName,
		tcpConfigMapKey:        tcpConfigMapName,
		acmeSecretKeyName:      acmeSecretKeyName,
		acmeTokenConfigmapName: acmeTokenConfigmapName,
		stateMutex:             sync.RWMutex{},
		updateQueue:            updateQueue,
		clear:                  true,
		needResync:             false,
	}
	// TODO I'm a circular reference, can you fix me?
	cache.listers = createListers(cache, logger, recorder, client, watchNamespace, resync)
	return cache
}

func (c *k8scache) RunAsync(stopCh <-chan struct{}) {
	c.listers.RunAsync(stopCh)
}

func (c *k8scache) GetIngressPodName() (namespace, podname string, err error) {
	namespace = os.Getenv("POD_NAMESPACE")
	podname = os.Getenv("POD_NAME")
	if namespace == "" || podname == "" {
		return "", "", fmt.Errorf("missing POD_NAMESPACE or POD_NAME envvar")
	}
	if pod, _ := c.client.CoreV1().Pods(namespace).Get(podname, metav1.GetOptions{}); pod == nil {
		return "", "", fmt.Errorf("ingress controller pod was not found: %s/%s", namespace, podname)
	}
	return namespace, podname, nil
}

func (c *k8scache) GetIngress(ingressName string) (*extensions.Ingress, error) {
	namespace, name, err := cache.SplitMetaNamespaceKey(ingressName)
	if err != nil {
		return nil, err
	}
	return c.listers.ingressLister.Ingresses(namespace).Get(name)
}

func (c *k8scache) GetIngressList() ([]*extensions.Ingress, error) {
	return c.listers.ingressLister.List(labels.Everything())
}

func (c *k8scache) GetService(serviceName string) (*api.Service, error) {
	namespace, name, err := cache.SplitMetaNamespaceKey(serviceName)
	if err != nil {
		return nil, err
	}
	return c.listers.serviceLister.Services(namespace).Get(name)
}

func (c *k8scache) GetSecret(secretName string) (*api.Secret, error) {
	namespace, name, err := cache.SplitMetaNamespaceKey(secretName)
	if err != nil {
		return nil, err
	}
	return c.listers.secretLister.Secrets(namespace).Get(name)
}

func (c *k8scache) GetConfigMap(configMapName string) (*api.ConfigMap, error) {
	namespace, name, err := cache.SplitMetaNamespaceKey(configMapName)
	if err != nil {
		return nil, err
	}
	return c.listers.configMapLister.ConfigMaps(namespace).Get(name)
}

func (c *k8scache) GetEndpoints(service *api.Service) (*api.Endpoints, error) {
	return c.listers.endpointLister.Endpoints(service.Namespace).Get(service.Name)
}

// GetTerminatingPods returns the pods that are terminating and belong
// (based on the Spec.Selector) to the supplied service.
func (c *k8scache) GetTerminatingPods(service *api.Service) (pl []*api.Pod, err error) {
	// converting the service selector to slice of string
	// in order to create the full match selector
	var ls []string
	for k, v := range service.Spec.Selector {
		ls = append(ls, fmt.Sprintf("%s=%s", k, v))
	}
	// parsing the label selector from the previous selectors
	l, err := labels.Parse(strings.Join(ls, ","))
	if err != nil {
		return nil, err
	}
	list, err := c.listers.podLister.Pods(service.Namespace).List(l)
	if err != nil {
		return nil, err
	}
	for _, p := range list {
		if isTerminatingPod(service, p) {
			pl = append(pl, p)
		}
	}
	return pl, nil
}

// isTerminatingPod Indicates whether or not pod belongs to svc, and is in the process of terminating
func isTerminatingPod(svc *api.Service, pod *api.Pod) bool {
	if svc.GetNamespace() != pod.GetNamespace() {
		return false
	}
	for selectorLabel, selectorValue := range svc.Spec.Selector {
		if labelValue, present := pod.Labels[selectorLabel]; !present || selectorValue != labelValue {
			return false
		}
	}
	if pod.DeletionTimestamp != nil && pod.Status.Reason != "NodeLost" && pod.Status.PodIP != "" {
		return true
	}
	return false
}

func (c *k8scache) GetPod(podName string) (*api.Pod, error) {
	namespace, name, err := cache.SplitMetaNamespaceKey(podName)
	if err != nil {
		return nil, err
	}
	return c.listers.podLister.Pods(namespace).Get(name)
}

func (c *k8scache) buildSecretName(defaultNamespace, secretName string) (string, string, error) {
	ns, name, err := cache.SplitMetaNamespaceKey(secretName)
	if err != nil {
		return "", "", err
	}
	if defaultNamespace == "" {
		return ns, name, nil
	}
	if ns == "" {
		return defaultNamespace, name, nil
	}
	if c.crossNS || ns == defaultNamespace {
		return ns, name, nil
	}
	return "", "", fmt.Errorf(
		"trying to read secret '%s' from namespace '%s', but cross-namespace reading is disabled; use --allow-cross-namespace to enable",
		secretName, defaultNamespace,
	)
}

func (c *k8scache) GetTLSSecretPath(defaultNamespace, secretName string) (file convtypes.CrtFile, err error) {
	namespace, name, err := c.buildSecretName(defaultNamespace, secretName)
	if err != nil {
		return file, err
	}
	sslCert, err := c.controller.GetCertificate(namespace, name)
	if err != nil {
		return file, err
	}
	if sslCert.PemFileName == "" {
		return file, fmt.Errorf("secret '%s/%s' does not have keys 'tls.crt' and 'tls.key'", namespace, name)
	}
	file = convtypes.CrtFile{
		Filename:   sslCert.PemFileName,
		SHA1Hash:   sslCert.PemSHA,
		CommonName: sslCert.Certificate.Subject.CommonName,
		NotAfter:   sslCert.Certificate.NotAfter,
	}
	return file, nil
}

func (c *k8scache) GetCASecretPath(defaultNamespace, secretName string) (ca, crl convtypes.File, err error) {
	namespace, name, err := c.buildSecretName(defaultNamespace, secretName)
	if err != nil {
		return ca, crl, err
	}
	sslCert, err := c.controller.GetCertificate(namespace, name)
	if err != nil {
		return ca, crl, err
	}
	if sslCert.CAFileName == "" {
		return ca, crl, fmt.Errorf("secret '%s/%s' does not have key 'ca.crt'", namespace, name)
	}
	ca = convtypes.File{
		Filename: sslCert.CAFileName,
		SHA1Hash: sslCert.PemSHA,
	}
	if sslCert.CRLFileName != "" {
		// ssl.AddCertAuth concatenates the hash of CA and CRL into the same attribute
		crl = convtypes.File{
			Filename: sslCert.CRLFileName,
			SHA1Hash: sslCert.PemSHA,
		}
	}
	return ca, crl, nil
}

func (c *k8scache) GetDHSecretPath(defaultNamespace, secretName string) (file convtypes.File, err error) {
	namespace, name, err := c.buildSecretName(defaultNamespace, secretName)
	if err != nil {
		return file, err
	}
	secret, err := c.listers.secretLister.Secrets(namespace).Get(name)
	if err != nil {
		return file, err
	}
	dh, found := secret.Data[dhparamFilename]
	if !found {
		return file, fmt.Errorf("secret '%s/%s' does not have key '%s'", namespace, name, dhparamFilename)
	}
	pem := fmt.Sprintf("%s_%s", namespace, name)
	pemFileName, err := ssl.AddOrUpdateDHParam(pem, dh)
	if err != nil {
		return file, fmt.Errorf("error creating dh-param file '%s': %v", pem, err)
	}
	file = convtypes.File{
		Filename: pemFileName,
		SHA1Hash: cfile.SHA1(pemFileName),
	}
	return file, nil
}

func (c *k8scache) GetSecretContent(defaultNamespace, secretName, keyName string) ([]byte, error) {
	namespace, name, err := c.buildSecretName(defaultNamespace, secretName)
	if err != nil {
		return nil, err
	}
	secret, err := c.listers.secretLister.Secrets(namespace).Get(name)
	if err != nil {
		return nil, err
	}
	data, found := secret.Data[keyName]
	if !found {
		return nil, fmt.Errorf("secret '%s/%s' does not have key '%s'", namespace, name, keyName)
	}
	return data, nil
}

// Implements acme.ClientResolver
func (c *k8scache) GetKey() (crypto.Signer, error) {
	secret, err := c.GetSecret(c.acmeSecretKeyName)
	var key *rsa.PrivateKey
	if err == nil {
		pemKey, found := secret.Data[api.TLSPrivateKeyKey]
		if !found {
			return nil, fmt.Errorf("secret '%s' does not have a key", c.acmeSecretKeyName)
		}
		derBlock, _ := pem.Decode(pemKey)
		if derBlock == nil {
			return nil, fmt.Errorf("secret '%s' has not a valid pem encoded private key", c.acmeSecretKeyName)
		}
		key, err = x509.ParsePKCS1PrivateKey(derBlock.Bytes)
		if err != nil {
			return nil, fmt.Errorf("error parsing acme client private key: %v", err)
		}
	}
	if key == nil {
		namespace, name, err := cache.SplitMetaNamespaceKey(c.acmeSecretKeyName)
		if err != nil {
			return nil, err
		}
		key, err = rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, err
		}
		pemEncode := pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(key),
		})
		newSecret := &api.Secret{}
		newSecret.Namespace = namespace
		newSecret.Name = name
		newSecret.Data = map[string][]byte{api.TLSPrivateKeyKey: pemEncode}
		if err := c.CreateOrUpdateSecret(newSecret); err != nil {
			return nil, err
		}
	}
	return key, nil
}

// Implements acme.SignerResolver
func (c *k8scache) GetTLSSecretContent(secretName string) *acme.TLSSecret {
	secret, err := c.GetSecret(secretName)
	if err != nil {
		return nil
	}
	pemCrt, foundCrt := secret.Data[api.TLSCertKey]
	pemKey, foundKey := secret.Data[api.TLSPrivateKeyKey]
	if !foundCrt || !foundKey {
		return nil
	}
	derCrt, _ := pem.Decode(pemCrt)
	derKey, _ := pem.Decode(pemKey)
	if derCrt == nil || derKey == nil {
		return nil
	}
	crt, errCrt := x509.ParseCertificate(derCrt.Bytes)
	key, errKey := x509.ParsePKCS1PrivateKey(derKey.Bytes)
	if errCrt != nil || errKey != nil {
		return nil
	}
	return &acme.TLSSecret{
		Crt: crt,
		Key: key,
	}
}

// Implements acme.SignerResolver
func (c *k8scache) SetTLSSecretContent(secretName string, pemCrt, pemKey []byte) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(secretName)
	if err != nil {
		return err
	}
	secret := &api.Secret{}
	secret.Namespace = namespace
	secret.Name = name
	secret.Type = api.SecretTypeTLS
	secret.Data = map[string][]byte{
		api.TLSCertKey:       pemCrt,
		api.TLSPrivateKeyKey: pemKey,
	}
	return c.CreateOrUpdateSecret(secret)
}

// Implements acme.ServerResolver
func (c *k8scache) GetToken(domain, uri string) string {
	config, err := c.GetConfigMap(c.acmeTokenConfigmapName)
	if err != nil {
		return ""
	}
	data, found := config.Data[domain]
	if !found {
		return ""
	}
	prefix := uri + "="
	if !strings.HasPrefix(data, prefix) {
		return ""
	}
	return strings.TrimPrefix(data, prefix)
}

// Implements acme.ClientResolver
func (c *k8scache) SetToken(domain string, uri, token string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(c.acmeTokenConfigmapName)
	if err != nil {
		return err
	}
	config, err := c.listers.configMapLister.ConfigMaps(namespace).Get(name)
	if err != nil {
		config = &api.ConfigMap{}
		config.Namespace = namespace
		config.Name = name
	}
	if config.Data == nil {
		config.Data = make(map[string]string, 1)
	}
	if token != "" {
		config.Data[domain] = uri + "=" + token
	} else {
		delete(config.Data, domain)
	}
	return c.CreateOrUpdateConfigMap(config)
}

func (c *k8scache) CreateOrUpdateSecret(secret *api.Secret) (err error) {
	cli := c.client.CoreV1().Secrets(secret.Namespace)
	if _, err := c.listers.secretLister.Secrets(secret.Namespace).Get(secret.Name); err != nil {
		_, err = cli.Create(secret)
	} else {
		_, err = cli.Update(secret)
	}
	return err
}

func (c *k8scache) CreateOrUpdateConfigMap(cm *api.ConfigMap) (err error) {
	cli := c.client.CoreV1().ConfigMaps(cm.Namespace)
	if _, err := c.listers.configMapLister.ConfigMaps(cm.Namespace).Get(cm.Name); err != nil {
		_, err = cli.Create(cm)
	} else {
		_, err = cli.Update(cm)
	}
	return err
}

// implements ListerEvents
func (c *k8scache) IsValidIngress(ing *extensions.Ingress) bool {
	return c.controller.IsValidClass(ing)
}

// implements ListerEvents
func (c *k8scache) IsValidConfigMap(cm *api.ConfigMap) bool {
	key := fmt.Sprintf("%s/%s", cm.Namespace, cm.Name)
	return key == c.globalConfigMapKey || key == c.tcpConfigMapKey
}

// implements ListerEvents
func (c *k8scache) Notify(old, cur interface{}) {
	// IMPLEMENT
	// maintain a list of changed objects only if partial parsing
	// is being used -- SyncNewObjects() is being called
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()
	if old == nil && cur == nil {
		c.needResync = true
	}
	if old != nil {
		switch old.(type) {
		case *extensions.Ingress:
			if cur == nil {
				c.delIngresses = append(c.delIngresses, old.(*extensions.Ingress))
			}
		case *api.Service:
			if cur == nil {
				c.delServices = append(c.delServices, old.(*api.Service))
			}
		case *api.Secret:
			if cur == nil {
				secret := old.(*api.Secret)
				c.delSecrets = append(c.delSecrets, secret)
				c.controller.DeleteSecret(fmt.Sprintf("%s/%s", secret.Namespace, secret.Name))
			}
		}
	}
	if cur != nil {
		switch cur.(type) {
		case *extensions.Ingress:
			ing := cur.(*extensions.Ingress)
			if old == nil {
				c.addIngresses = append(c.addIngresses, ing)
			} else {
				c.updIngresses = append(c.updIngresses, ing)
			}
		case *api.Endpoints:
			c.newEndpoints = append(c.newEndpoints, cur.(*api.Endpoints))
		case *api.Service:
			svc := cur.(*api.Service)
			if old == nil {
				c.addServices = append(c.addServices, svc)
			} else {
				c.updServices = append(c.updServices, svc)
			}
		case *api.Secret:
			secret := cur.(*api.Secret)
			if old == nil {
				c.addSecrets = append(c.addSecrets, secret)
			} else {
				c.updSecrets = append(c.updSecrets, secret)
			}
		case *api.ConfigMap:
			cm := cur.(*api.ConfigMap)
			key := fmt.Sprintf("%s/%s", cm.Namespace, cm.Name)
			switch key {
			case c.globalConfigMapKey:
				c.newGlobalConfigMapData = cm.Data
			case c.tcpConfigMapKey:
				c.newTCPConfigMapData = cm.Data
			}
		case *api.Pod:
			c.newPods = append(c.newPods, cur.(*api.Pod))
		}
	}
	if c.clear {
		// Notify after 500ms, giving the time to receive
		// all/most of the changes of a batch update
		// TODO parameterize this delay
		time.AfterFunc(500*time.Millisecond, func() { c.updateQueue.Notify() })
	}
	c.clear = false
}

// implements converters.types.Cache
func (c *k8scache) NeedResync() bool {
	c.stateMutex.RLock()
	defer c.stateMutex.RUnlock()
	return c.needResync
}

// implements converters.types.Cache
func (c *k8scache) GlobalConfig() (cur, new map[string]string) {
	return c.globalConfigMapData, c.newGlobalConfigMapData
}

// implements converters.types.Cache
func (c *k8scache) GetDirtyIngresses() (del, upd, add []*extensions.Ingress) {
	c.stateMutex.RLock()
	defer c.stateMutex.RUnlock()
	del = make([]*extensions.Ingress, len(c.delIngresses))
	for i := range c.delIngresses {
		del[i] = c.delIngresses[i]
	}
	upd = make([]*extensions.Ingress, len(c.updIngresses))
	for i := range c.updIngresses {
		upd[i] = c.updIngresses[i]
	}
	add = make([]*extensions.Ingress, len(c.addIngresses))
	for i := range c.addIngresses {
		add[i] = c.addIngresses[i]
	}
	return del, upd, add
}

// implements converters.types.Cache
func (c *k8scache) GetDirtyEndpoints() []*api.Endpoints {
	c.stateMutex.RLock()
	defer c.stateMutex.RUnlock()
	ep := make([]*api.Endpoints, len(c.newEndpoints))
	for i := range c.newEndpoints {
		ep[i] = c.newEndpoints[i]
	}
	return ep
}

// implements converters.types.Cache
func (c *k8scache) GetDirtyServices() (del, upd, add []*api.Service) {
	c.stateMutex.RLock()
	defer c.stateMutex.RUnlock()
	del = make([]*api.Service, len(c.delServices))
	for i := range c.delServices {
		del[i] = c.delServices[i]
	}
	upd = make([]*api.Service, len(c.updServices))
	for i := range c.updServices {
		upd[i] = c.updServices[i]
	}
	add = make([]*api.Service, len(c.addServices))
	for i := range c.addServices {
		add[i] = c.addServices[i]
	}
	return del, upd, add
}

// implements converters.types.Cache
func (c *k8scache) GetDirtySecrets() (del, upd, add []*api.Secret) {
	c.stateMutex.RLock()
	defer c.stateMutex.RUnlock()
	del = make([]*api.Secret, len(c.delSecrets))
	for i := range c.delSecrets {
		del[i] = c.delSecrets[i]
	}
	upd = make([]*api.Secret, len(c.updSecrets))
	for i := range c.updSecrets {
		upd[i] = c.updSecrets[i]
	}
	add = make([]*api.Secret, len(c.addSecrets))
	for i := range c.addSecrets {
		add[i] = c.addSecrets[i]
	}
	return del, upd, add
}

// implements converters.types.Cache
func (c *k8scache) GetDirtyPods() []*api.Pod {
	c.stateMutex.RLock()
	defer c.stateMutex.RUnlock()
	pods := make([]*api.Pod, len(c.newPods))
	for i := range c.newPods {
		pods[i] = c.newPods[i]
	}
	return pods
}

// implements converters.types.Cache
func (c *k8scache) SyncNewObjects() {
	// IMPLEMENT
	// lock between the first state reading and this sync
	// this will avoid loose unread state change
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()
	//
	c.newPods = nil
	c.newEndpoints = nil
	//
	// Secrets
	//
	c.delSecrets = nil
	c.updSecrets = nil
	c.addSecrets = nil
	//
	// Ingress
	//
	c.delIngresses = nil
	c.updIngresses = nil
	c.addIngresses = nil
	//
	// ConfigMaps
	//
	if c.newGlobalConfigMapData != nil {
		c.globalConfigMapData = c.newGlobalConfigMapData
		c.newGlobalConfigMapData = nil
	}
	if c.newTCPConfigMapData != nil {
		c.tcpConfigMapData = c.newTCPConfigMapData
		c.newTCPConfigMapData = nil
	}
	//
	c.clear = true
	c.needResync = false
}
