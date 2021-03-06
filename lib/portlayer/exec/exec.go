// Copyright 2016 VMware, Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package exec

import (
	"fmt"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/vic/lib/portlayer/event"
	"github.com/vmware/vic/lib/portlayer/event/collector/vsphere"
	"github.com/vmware/vic/lib/portlayer/event/events"
	"github.com/vmware/vic/pkg/trace"
	"github.com/vmware/vic/pkg/vsphere/extraconfig"
	"github.com/vmware/vic/pkg/vsphere/session"
	"golang.org/x/net/context"
)

var initializer sync.Once

func Init(ctx context.Context, sess *session.Session, source extraconfig.DataSource, _ extraconfig.DataSink) error {
	var err error
	initializer.Do(func() {
		f := find.NewFinder(sess.Vim25(), false)

		extraconfig.Decode(source, &Config)

		log.Debugf("Decoded VCH config for execution: %#v", Config)
		ccount := len(Config.ComputeResources)
		if ccount != 1 {
			err = fmt.Errorf("expected singular compute resource element, found %d", ccount)
			log.Error(err)
			return
		}

		cr := Config.ComputeResources[0]
		var r object.Reference
		r, err = f.ObjectReference(ctx, cr)
		if err != nil {
			err = fmt.Errorf("could not get resource pool or virtual app reference from %q: %s", cr.String(), err)
			log.Error(err)
			return
		}
		switch o := r.(type) {
		case *object.VirtualApp:
			Config.VirtualApp = o
			Config.ResourcePool = o.ResourcePool
		case *object.ResourcePool:
			Config.ResourcePool = o
		default:
			err = fmt.Errorf("could not get resource pool or virtual app from reference %q: object type is wrong", cr.String())
			log.Error(err)
			return
		}

		// we want to monitor the cluster, so create a vSphere Event Collector
		// The cluster managed object will either be a proper vSphere Cluster or
		// a specific host when standalone mode
		ec := vsphere.NewCollector(sess.Vim25(), sess.Cluster.Reference().String())

		// start the collection of vsphere events
		err = ec.Start()
		if err != nil {
			err = fmt.Errorf("%s failed to start: %s", ec.Name(), err)
			log.Error(err)
			return
		}

		// create the event manager &  register the existing collector
		Config.EventManager = event.NewEventManager(ec)

		// subscribe the exec layer to the event stream for Vm events
		Config.EventManager.Subscribe(events.NewEventType(vsphere.VmEvent{}).Topic(), "exec", eventCallback)

		// instantiate the container cache now
		NewContainerCache()

		//FIXME: temporary injection of debug network for debug nic
		ne := Config.Networks["client"]
		if ne == nil {
			err = fmt.Errorf("could not get client network reference for debug nic - this code can be removed once network mapping/dhcp client is present")
			log.Error(err)
			return
		}

		nr := new(types.ManagedObjectReference)
		nr.FromString(ne.Network.ID)
		r, err = f.ObjectReference(ctx, *nr)
		if err != nil {
			err = fmt.Errorf("could not get client network reference from %s: %s", nr.String(), err)
			log.Error(err)
			return
		}
		Config.DebugNetwork = r.(object.NetworkReference)

		// Grab the AboutInfo about our host environment
		about := sess.Vim25().ServiceContent.About
		Config.VCHMhz = NCPU(ctx)
		Config.VCHMemoryLimit = MemTotal(ctx)
		Config.HostOS = about.OsType
		Config.HostOSVersion = about.Version
		Config.HostProductName = about.Name
		log.Debugf("Host - OS (%s), version (%s), name (%s)", about.OsType, about.Version, about.Name)
		log.Debugf("VCH limits - %d Mhz, %d MB", Config.VCHMhz, Config.VCHMemoryLimit)

		// sync container cache
		if err = Containers.sync(ctx, sess); err != nil {
			return
		}
	})

	return err
}

// eventCallback will process events
func eventCallback(ie events.Event) {
	// grab the container from the cache
	container := Containers.Container(ie.Reference())
	if container != nil {

		newState := eventedState(ie.String(), container.State)
		// do we have a state change
		if newState != container.State {
			switch newState {
			case StateStopping,
				StateRunning,
				StateStopped,
				StateSuspended:

				log.Debugf("Container(%s) state set to %s via event activity", container.ExecConfig.ID, newState.String())
				container.State = newState

				if newState == StateStopped {
					container.onStop()
				}

				// container state has changed so we need to update the container attributes
				// we'll do this in a go routine to avoid blocking
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), propertyCollectorTimeout)
					defer cancel()

					_, err := container.Update(ctx, container.vm.Session)
					if err != nil {
						log.Errorf("Event driven container update failed: %s", err.Error())
					}
					// regardless of update success failure publish the container event
					publishContainerEvent(container.ExecConfig.ID, ie.Created(), ie.String())
				}()
			case StateRemoved:
				log.Debugf("Container(%s) %s via event activity", container.ExecConfig.ID, newState.String())
				Containers.Remove(container.ExecConfig.ID)
				publishContainerEvent(container.ExecConfig.ID, ie.Created(), ie.String())

			}
		}
	}

	return
}

// eventedState will determine the target container
// state based on the current container state and the vsphere event
func eventedState(e string, current State) State {
	switch e {
	case events.ContainerPoweredOn:
		// are we in the process of starting
		if current != StateStarting {
			return StateRunning
		}
	case events.ContainerPoweredOff:
		// are we in the process of stopping
		if current != StateStopping {
			return StateStopped
		}
	case events.ContainerSuspended:
		// are we in the process of suspending
		if current != StateSuspending {
			return StateSuspended
		}
	case events.ContainerRemoved:
		if current != StateRemoving {
			return StateRemoved
		}
	}
	return current
}

// publishContainerEvent will publish a ContainerEvent to the vic event stream
func publishContainerEvent(id string, created time.Time, eventType string) {
	if Config.EventManager == nil || eventType == "" {
		return
	}

	ce := &events.ContainerEvent{
		BaseEvent: &events.BaseEvent{
			Ref:         id,
			CreatedTime: created,
			Event:       eventType,
			Detail:      fmt.Sprintf("Container %s %s", id, eventType),
		},
	}

	Config.EventManager.Publish(ce)
}

func WaitForContainerStop(ctx context.Context, id string) error {
	defer trace.End(trace.Begin(id))

	listen := make(chan interface{})
	defer close(listen)

	watch := func(ce events.Event) {
		event := ce.String()
		if ce.Reference() == id {
			switch event {
			case events.ContainerStopped,
				events.ContainerPoweredOff:
				listen <- event
			}
		}
	}

	sub := fmt.Sprintf("%s:%s(%d)", id, "watcher", &watch)
	topic := events.NewEventType(events.ContainerEvent{}).Topic()
	Config.EventManager.Subscribe(topic, sub, watch)
	defer Config.EventManager.Unsubscribe(topic, sub)

	// wait for the event to be pushed on the channel or
	// the context to be complete
	select {
	case <-listen:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("WaitForContainerStop(%s) Error: %s", id, ctx.Err())
	}
}
