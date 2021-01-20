/*
Copyright 2015 The Kubernetes Authors.

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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/kong/deck/diff"
	"github.com/kong/deck/dump"
	"github.com/kong/deck/file"
	"github.com/kong/deck/solver"
	"github.com/kong/deck/state"
	deckutils "github.com/kong/deck/utils"
	"github.com/kong/go-kong/kong"
	"github.com/kong/kubernetes-ingress-controller/internal/ingress/controller/parser/kongstate"
	"github.com/kong/kubernetes-ingress-controller/internal/ingress/utils"
)

// OnUpdate is called periodically by syncQueue to keep the configuration in sync.
// returning nil implies the synchronization finished correctly.
// Returning an error means requeue the update.
func (n *KongController) OnUpdate(ctx context.Context, state *kongstate.KongState) error {
	targetContent := n.toDeckContent(ctx, state)

	var customEntities []byte
	var err error
	// process any custom entities
	if n.cfg.InMemory && n.cfg.KongCustomEntitiesSecret != "" {
		customEntities, err = n.fetchCustomEntities()
		if err != nil {
			// failure to fetch custom entities shouldn't block updates
			n.Logger.Errorf("failed to fetch custom entities: %v", err)
		}
	}

	var shaSum []byte
	// disable optimization if reverse sync is enabled
	if !n.cfg.EnableReverseSync {
		shaSum, err = generateSHA(targetContent, customEntities)
		if err != nil {
			return err
		}
		if reflect.DeepEqual(n.runningConfigHash, shaSum) {
			n.Logger.Info("no configuration change, skipping sync to kong")
			return nil
		}
	}
	if n.cfg.InMemory {
		err = n.onUpdateInMemoryMode(ctx, targetContent, customEntities)
	} else {
		err = n.onUpdateDBMode(targetContent)
	}
	if err != nil {
		return err
	}
	n.runningConfigHash = shaSum
	n.Logger.Info("successfully synced configuration to kong")
	return nil
}

func generateSHA(targetContent *file.Content,
	customEntities []byte) ([]byte, error) {

	var buffer bytes.Buffer

	jsonConfig, err := json.Marshal(targetContent)
	if err != nil {
		return nil, fmt.Errorf("marshaling Kong declarative configuration to JSON: %w", err)
	}
	buffer.Write(jsonConfig)

	if customEntities != nil {
		buffer.Write(customEntities)
	}

	shaSum := sha256.Sum256(buffer.Bytes())
	return shaSum[:], nil
}

func cleanUpNullsInPluginConfigs(state *file.Content) {

	for _, s := range state.Services {
		for _, p := range s.Plugins {
			for k, v := range p.Config {
				if v == nil {
					delete(p.Config, k)
				}
			}
		}
		for _, r := range state.Routes {
			for _, p := range r.Plugins {
				for k, v := range p.Config {
					if v == nil {
						delete(p.Config, k)
					}
				}
			}
		}
	}

	for _, c := range state.Consumers {
		for _, p := range c.Plugins {
			for k, v := range p.Config {
				if v == nil {
					delete(p.Config, k)
				}
			}
		}
	}

	for _, p := range state.Plugins {
		for k, v := range p.Config {
			if v == nil {
				delete(p.Config, k)
			}
		}
	}
}

func (n *KongController) renderConfigWithCustomEntities(state *file.Content,
	customEntitiesJSONBytes []byte) ([]byte, error) {

	var kongCoreConfig []byte
	var err error

	kongCoreConfig, err = json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("marshaling kong config into json: %w", err)
	}

	// fast path
	if len(customEntitiesJSONBytes) == 0 {
		return kongCoreConfig, nil
	}

	// slow path
	mergeMap := map[string]interface{}{}
	var result []byte
	var customEntities map[string]interface{}

	// unmarshal core config into the merge map
	err = json.Unmarshal(kongCoreConfig, &mergeMap)
	if err != nil {
		return nil, fmt.Errorf("unmarshalling kong config into map[string]interface{}: %w", err)
	}

	// unmarshal custom entities config into the merge map
	err = json.Unmarshal(customEntitiesJSONBytes, &customEntities)
	if err != nil {
		// do not error out when custom entities are messed up
		n.Logger.Errorf("failed to unmarshal custom entities from secret data: %v", err)
	} else {
		for k, v := range customEntities {
			if _, exists := mergeMap[k]; !exists {
				mergeMap[k] = v
			}
		}
	}

	// construct the final configuration
	result, err = json.Marshal(mergeMap)
	if err != nil {
		err = fmt.Errorf("marshaling final config into JSON: %w", err)
		return nil, err
	}

	return result, nil
}

func (n *KongController) fetchCustomEntities() ([]byte, error) {
	ns, name, err := utils.ParseNameNS(n.cfg.KongCustomEntitiesSecret)
	if err != nil {
		return nil, fmt.Errorf("parsing kong custom entities secret: %w", err)
	}
	secret, err := n.store.GetSecret(ns, name)
	if err != nil {
		return nil, fmt.Errorf("fetching secret: %w", err)
	}
	config, ok := secret.Data["config"]
	if !ok {
		return nil, fmt.Errorf("'config' key not found in "+
			"custom entities secret '%v'", n.cfg.KongCustomEntitiesSecret)
	}
	return config, nil
}

func (n *KongController) onUpdateInMemoryMode(ctx context.Context,
	state *file.Content,
	customEntities []byte) error {
	client := n.cfg.Kong.Client

	// Kong will error out if this is set
	state.Info = nil
	// Kong errors out if `null`s are present in `config` of plugins
	cleanUpNullsInPluginConfigs(state)

	config, err := n.renderConfigWithCustomEntities(state, customEntities)
	if err != nil {
		return fmt.Errorf("constructing kong configuration: %w", err)
	}

	req, err := http.NewRequest("POST", n.cfg.Kong.URL+"/config",
		bytes.NewReader(config))
	if err != nil {
		return fmt.Errorf("creating new HTTP request for /config: %w", err)
	}
	req.Header.Add("content-type", "application/json")

	queryString := req.URL.Query()
	queryString.Add("check_hash", "1")

	req.URL.RawQuery = queryString.Encode()

	_, err = client.Do(ctx, req, nil)
	if err != nil {
		return fmt.Errorf("posting new config to /config: %w", err)
	}

	return err
}

func (n *KongController) onUpdateDBMode(targetContent *file.Content) error {
	client := n.cfg.Kong.Client

	// read the current state
	rawState, err := dump.Get(client, dump.Config{
		SelectorTags: n.getIngressControllerTags(),
	})
	if err != nil {
		return fmt.Errorf("loading configuration from kong: %w", err)
	}
	currentState, err := state.Get(rawState)
	if err != nil {
		return err
	}

	// read the target state
	rawState, err = file.Get(targetContent, file.RenderConfig{
		CurrentState: currentState,
		KongVersion:  n.cfg.Kong.Version,
	})
	if err != nil {
		return err
	}
	targetState, err := state.Get(rawState)
	if err != nil {
		return err
	}

	syncer, err := diff.NewSyncer(currentState, targetState)
	if err != nil {
		return fmt.Errorf("creating a new syncer: %w", err)
	}
	syncer.SilenceWarnings = true
	//client.SetDebugMode(true)
	_, errs := solver.Solve(nil, syncer, client, n.cfg.Kong.Concurrency, false)
	if errs != nil {
		return deckutils.ErrArray{Errors: errs}
	}
	return nil
}

// getIngressControllerTags returns a tag to use if the current
// Kong entity supports tagging.
func (n *KongController) getIngressControllerTags() []string {
	var res []string
	if n.cfg.Kong.HasTagSupport {
		res = append(res, n.cfg.Kong.FilterTags...)
	}
	return res
}

const FormatVersion = "1.1"

func (n *KongController) toDeckContent(
	ctx context.Context,
	k8sState *kongstate.KongState) *file.Content {
	var content file.Content
	content.FormatVersion = FormatVersion
	var err error

	for _, s := range k8sState.Services {
		service := file.FService{Service: s.Service}
		for _, p := range s.Plugins {
			plugin := file.FPlugin{
				Plugin: *p.DeepCopy(),
			}
			err = n.fillPlugin(ctx, &plugin)
			if err != nil {
				n.Logger.Errorf("failed to fill-in defaults for plugin: %s", *plugin.Name)
			}
			service.Plugins = append(service.Plugins, &plugin)
			sortByString(service.Plugins, func(i int) string { return *service.Plugins[i].Name })
		}

		for _, r := range s.Routes {
			route := file.FRoute{Route: r.Route}
			n.fillRoute(&route.Route)

			for _, p := range r.Plugins {
				plugin := file.FPlugin{
					Plugin: *p.DeepCopy(),
				}
				err = n.fillPlugin(ctx, &plugin)
				if err != nil {
					n.Logger.Errorf("failed to fill-in defaults for plugin: %s", *plugin.Name)
				}
				route.Plugins = append(route.Plugins, &plugin)
				sortByString(route.Plugins, func(i int) string { return *route.Plugins[i].Name })
			}
			service.Routes = append(service.Routes, &route)
		}
		sortByString(service.Routes, func(i int) string { return *service.Routes[i].Name })
		content.Services = append(content.Services, service)
	}
	sortByString(content.Services, func(i int) string { return *content.Services[i].Name })

	for _, plugin := range k8sState.Plugins {
		plugin := file.FPlugin{
			Plugin: plugin.Plugin,
		}
		err = n.fillPlugin(ctx, &plugin)
		if err != nil {
			n.Logger.Errorf("failed to fill-in defaults for plugin: %s", *plugin.Name)
		}
		content.Plugins = append(content.Plugins, plugin)
	}
	sortByString(content.Plugins, func(i int) string { return pluginString(content.Plugins[i]) })

	for _, u := range k8sState.Upstreams {
		n.fillUpstream(&u.Upstream)
		upstream := file.FUpstream{Upstream: u.Upstream}
		for _, t := range u.Targets {
			target := file.FTarget{Target: t.Target}
			upstream.Targets = append(upstream.Targets, &target)
		}
		sortByString(upstream.Targets, func(i int) string { return *upstream.Targets[i].Target.Target })
		content.Upstreams = append(content.Upstreams, upstream)
	}
	sortByString(content.Upstreams, func(i int) string { return *content.Upstreams[i].Name })

	for _, c := range k8sState.Certificates {
		cert := getFCertificateFromKongCert(c.Certificate)
		content.Certificates = append(content.Certificates, cert)
	}
	sortByString(content.Certificates, func(i int) string { return *content.Certificates[i].Cert })

	for _, c := range k8sState.CACertificates {
		content.CACertificates = append(content.CACertificates,
			file.FCACertificate{CACertificate: c})
	}
	sortByString(content.CACertificates, func(i int) string { return *content.CACertificates[i].Cert })

	for _, c := range k8sState.Consumers {
		consumer := file.FConsumer{Consumer: c.Consumer}
		for _, p := range c.Plugins {
			consumer.Plugins = append(consumer.Plugins, &file.FPlugin{Plugin: p})
		}

		for k := range c.KeyAuths {
			consumer.KeyAuths = append(consumer.KeyAuths, c.KeyAuths[k])
		}
		sortByString(consumer.KeyAuths, func(i int) string { return *consumer.KeyAuths[i].Key })
		for k := range c.HMACAuths {
			consumer.HMACAuths = append(consumer.HMACAuths, c.HMACAuths[k])
		}
		sortByString(consumer.HMACAuths, func(i int) string { return *consumer.HMACAuths[i].Username })
		for k := range c.BasicAuths {
			consumer.BasicAuths = append(consumer.BasicAuths, c.BasicAuths[k])
		}
		sortByString(consumer.BasicAuths, func(i int) string { return *consumer.BasicAuths[i].Username })
		for k := range c.JWTAuths {
			consumer.JWTAuths = append(consumer.JWTAuths, c.JWTAuths[k])
		}
		sortByString(consumer.JWTAuths, func(i int) string { return *consumer.JWTAuths[i].Key })
		for k := range c.Oauth2Creds {
			consumer.Oauth2Creds = append(consumer.Oauth2Creds, c.Oauth2Creds[k])
		}
		sortByString(consumer.Oauth2Creds, func(i int) string { return *consumer.Oauth2Creds[i].ClientID })
		content.Consumers = append(content.Consumers, consumer)
	}
	sortByString(content.Consumers, func(i int) string { return *content.Consumers[i].Username })

	selectorTags := n.getIngressControllerTags()
	if len(selectorTags) > 0 {
		content.Info = &file.Info{
			SelectorTags: selectorTags,
		}
	}

	return &content
}

func sortByString(slice interface{}, fieldFn func(i int) string) {
	lessFn := func(i, j int) bool { return strings.Compare(fieldFn(i), fieldFn(j)) < 0 }
	sort.SliceStable(slice, lessFn)
}

func getFCertificateFromKongCert(kongCert kong.Certificate) file.FCertificate {
	var res file.FCertificate
	if kongCert.ID != nil {
		res.ID = kong.String(*kongCert.ID)
	}
	if kongCert.Key != nil {
		res.Key = kong.String(*kongCert.Key)
	}
	if kongCert.Cert != nil {
		res.Cert = kong.String(*kongCert.Cert)
	}
	res.SNIs = getSNIs(kongCert.SNIs)
	return res
}

func getSNIs(names []*string) []kong.SNI {
	var snis []kong.SNI
	for _, name := range names {
		snis = append(snis, kong.SNI{
			Name: kong.String(*name),
		})
	}
	return snis
}

func pluginString(plugin file.FPlugin) string {
	result := ""
	if plugin.Name != nil {
		result = *plugin.Name
	}
	if plugin.Consumer != nil && plugin.Consumer.ID != nil {
		result += *plugin.Consumer.ID
	}
	if plugin.Route != nil && plugin.Route.ID != nil {
		result += *plugin.Route.ID
	}
	if plugin.Service != nil && plugin.Service.ID != nil {
		result += *plugin.Service.ID
	}
	return result
}

func (n *KongController) fillRoute(route *kong.Route) {
	if route.HTTPSRedirectStatusCode == nil {
		route.HTTPSRedirectStatusCode = kong.Int(426)
	}
	if route.PathHandling == nil {
		route.PathHandling = kong.String("v0")
	}
}

func (n *KongController) fillUpstream(upstream *kong.Upstream) {
	if upstream.Algorithm == nil {
		upstream.Algorithm = kong.String("round-robin")
	}
}

func (n *KongController) fillPlugin(ctx context.Context, plugin *file.FPlugin) error {
	if plugin == nil {
		return fmt.Errorf("plugin is nil")
	}
	if plugin.Name == nil || *plugin.Name == "" {
		return fmt.Errorf("plugin doesn't have a name")
	}
	schema, err := n.PluginSchemaStore.Schema(ctx, *plugin.Name)
	if err != nil {
		return fmt.Errorf("error retrieveing schema for plugin %s: %w", *plugin.Name, err)
	}
	if plugin.Config == nil {
		plugin.Config = make(kong.Configuration)
	}
	newConfig, err := fill(schema, plugin.Config)
	if err != nil {
		return fmt.Errorf("error filling in default for plugin %s: %w", *plugin.Name, err)
	}
	plugin.Config = newConfig
	if plugin.RunOn == nil {
		plugin.RunOn = kong.String("first")
	}
	if plugin.Enabled == nil {
		plugin.Enabled = kong.Bool(true)
	}
	if len(plugin.Protocols) == 0 {
		// TODO read this from the schema endpoint
		plugin.Protocols = kong.StringSlice("http", "https")
	}
	plugin.RunOn = nil
	return nil
}
