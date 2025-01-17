//
// This file is part of mdns-discovery.
//
// Copyright 2018-2021 ARDUINO SA (http://www.arduino.cc/)
//
// This software is released under the GNU General Public License version 3,
// which covers the main part of arduino-cli.
// The terms of this license can be found at:
// https://www.gnu.org/licenses/gpl-3.0.en.html
//
// You can be released from the requirements of the above licenses by purchasing
// a commercial license. Buying such a license is mandatory if you want to modify or
// otherwise use the software for commercial activities involving the Arduino
// software without disclosing the source code of your own applications. To purchase
// a commercial license, send an email to license@arduino.cc.
//

package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	properties "github.com/arduino/go-properties-orderedmap"
	discovery "github.com/arduino/pluggable-discovery-protocol-handler/v2"
	"github.com/hashicorp/mdns"
)

func main() {
	parseArgs()
	mdnsDiscovery := &MDNSDiscovery{}
	disc := discovery.NewServer(mdnsDiscovery)
	if err := disc.Run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err.Error())
		os.Exit(1)
	}
}

const mdnsServiceName = "_arduino._tcp"

// Discovered ports stay alive for this amount of time
// since the last time they've been found by an mDNS query.
const portsTTL = time.Second * 60

// This is interval at which mDNS queries are made.
const discoveryInterval = time.Second * 15

// IP address used to check if we're connected to a local network
var ipv4Addr = &net.UDPAddr{
	IP:   net.ParseIP("224.0.0.251"),
	Port: 5353,
}

// IP address used to check if IPv6 is supported by the local network
var ipv6Addr = &net.UDPAddr{
	IP:   net.ParseIP("ff02::fb"),
	Port: 5353,
}

// MDNSDiscovery is the implementation of the network pluggable-discovery
type MDNSDiscovery struct {
	cancelFunc  func()
	entriesChan chan *mdns.ServiceEntry

	portsCache *portsCache
}

// Hello handles the pluggable-discovery HELLO command
func (d *MDNSDiscovery) Hello(userAgent string, protocolVersion int) error {
	// The mdns library used has some logs statement that we must disable
	log.SetOutput(ioutil.Discard)
	return nil
}

// Stop handles the pluggable-discovery STOP command
func (d *MDNSDiscovery) Stop() error {
	if d.cancelFunc != nil {
		d.cancelFunc()
		d.cancelFunc = nil
	}
	if d.entriesChan != nil {
		close(d.entriesChan)
		d.entriesChan = nil
	}
	if d.portsCache != nil {
		d.portsCache.clear()
	}
	return nil
}

// Quit handles the pluggable-discovery QUIT command
func (d *MDNSDiscovery) Quit() {
	d.Stop()
}

// StartSync handles the pluggable-discovery START_SYNC command
func (d *MDNSDiscovery) StartSync(eventCB discovery.EventCallback, errorCB discovery.ErrorCallback) error {
	if d.entriesChan != nil {
		return fmt.Errorf("already syncing")
	}

	if d.portsCache == nil {
		// Initialize the cache if not already done
		d.portsCache = newCache(portsTTL, func(port *discovery.Port) {
			eventCB("remove", port)
		})
	}

	d.entriesChan = make(chan *mdns.ServiceEntry)
	go func() {
		for entry := range d.entriesChan {
			port := toDiscoveryPort(entry)
			if port == nil {
				continue
			}
			if updated := d.portsCache.storeOrUpdate(port); !updated {
				// Port is not cached so let the user know a new one has been found
				eventCB("add", port)
			}
		}
	}()

	// We use a separate channel to consume the events received
	// from Query and send them over to d.entriesChan only if
	// it's open.
	// If we'd have used d.entriesChan to get the events from
	// Query we risk panics cause of sends to a closed channel.
	// Query doesn't stop right away when we call d.Stop()
	// neither we have to any to do it, we can only wait for it
	// to return.
	queriesChan := make(chan *mdns.ServiceEntry)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(queriesChan)

		disableIPv6 := false
		// Check if the current network supports IPv6
		mconn6, err := net.ListenMulticastUDP("udp6", nil, ipv6Addr)
		if err != nil {
			disableIPv6 = true
		} else {
			mconn6.Close()
		}

		// We must check if we're connected to a local network, if we don't
		// the subsequent mDNS query would fail and return an error.
		mconn4, err := net.ListenMulticastUDP("udp4", nil, ipv4Addr)
		if err != nil {
			return
		}
		// If we managed to open a connection close it, mdns.Query opens
		// another one on the same IP address we use and it would fail
		// if we leave this open.
		mconn4.Close()

		params := &mdns.QueryParam{
			Service:             mdnsServiceName,
			Domain:              "local",
			Timeout:             discoveryInterval,
			Entries:             queriesChan,
			WantUnicastResponse: false,
			DisableIPv6:         disableIPv6,
		}
		for {
			if err := mdns.Query(params); err != nil {
				errorCB("mdns lookup error: " + err.Error())
			}
			select {
			default:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		for entry := range queriesChan {
			if d.entriesChan != nil {
				d.entriesChan <- entry
			}
		}
	}()
	d.cancelFunc = cancel
	return nil
}

func toDiscoveryPort(entry *mdns.ServiceEntry) *discovery.Port {
	// Only entries that match the Arduino OTA service name must
	// be returned
	if !strings.HasSuffix(entry.Name, mdnsServiceName+".local.") {
		return nil
	}

	ip := ""
	if len(entry.AddrV4) > 0 {
		ip = entry.AddrV4.String()
	} else if len(entry.AddrV6) > 0 {
		ip = entry.AddrV6.String()
	}

	props := properties.NewMap()
	props.Set("hostname", entry.Host)
	props.Set("port", strconv.Itoa(entry.Port))

	for _, field := range entry.InfoFields {
		split := strings.Split(field, "=")
		if len(split) != 2 {
			continue
		}
		key, value := split[0], split[1]
		props.Set(key, value)
		if key == "board" {
			// duplicate for backwards compatibility
			props.Set(".", value)
		}
	}

	var name string
	if split := strings.Split(entry.Host, "."); len(split) > 0 {
		// Use the first part of the entry host name to display
		// the address label
		name = split[0]
	} else {
		// Fallback
		name = entry.Name
	}

	return &discovery.Port{
		Address:       ip,
		AddressLabel:  fmt.Sprintf("%s at %s", name, ip),
		Protocol:      "network",
		ProtocolLabel: "Network Port",
		Properties:    props,
	}
}
