// Copyright 2022 Google LLC
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

package route_removal_via_flush_test

import (
	"context"
	"testing"
	"time"

	"github.com/openconfig/featureprofiles/internal/attrs"
	"github.com/openconfig/featureprofiles/internal/fptest"
	gpb "github.com/openconfig/gribi/v1/proto/service"
	"github.com/openconfig/gribigo/chk"
	"github.com/openconfig/gribigo/fluent"
	"github.com/openconfig/ondatra"
	"github.com/openconfig/ondatra/telemetry/ateflow"
)

func TestMain(m *testing.M) {
	fptest.RunTests(m)
}

// Settings for configuring the baseline testbed with the test
// topology.
//
// The testbed consists of ate:port1 -> dut:port1,
// dut:port2 -> ate:port2.
//
//   * ate:port1 -> dut:port1 subnet 192.0.2.0/30
//   * ate:port2 -> dut:port2 subnet 192.0.2.4/30
//
//   * Destination network: 198.51.100.0/24

const (
	ateDstNetCIDR            = "198.51.100.0/24"
	defaultNetworkInstance   = "default"
	clientAOriginElectionID  = 10
	clientBOriginElectionID  = 9
	clientAUpdatedElectionID = 12
	clientBUpdatedElectionID = 11
)

var (
	dutPort1 = attrs.Attributes{
		Desc:    "dutPort1",
		IPv4:    "192.0.2.1",
		IPv4Len: 30,
	}

	atePort1 = attrs.Attributes{
		Name:    "atePort1",
		IPv4:    "192.0.2.2",
		IPv4Len: 30,
	}

	dutPort2 = attrs.Attributes{
		Desc:    "dutPort2",
		IPv4:    "192.0.2.5",
		IPv4Len: 30,
	}

	atePort2 = attrs.Attributes{
		Name:    "atePort2",
		IPv4:    "192.0.2.6",
		IPv4Len: 30,
	}
)

// TestRouteRemovelViaFlush test flush with the following operations
// 1. Flush request from clientA (the primary client) should succeed.
// 2. Flush request from clientB (not a primary client) should fail.
// 3. Failover the primary role from clientA to clientB.
// 4. Flush from clientB should succeed.
func TestRouteRemovelViaFlush(t *testing.T) {
	ctx := context.Background()

	dut := ondatra.DUT(t, "dut")
	configureDUT(t, dut)

	ate := ondatra.ATE(t, "ate")
	ateTop := configureATE(t, ate)
	ateTop.Push(t).StartProtocols(t)

	gribic := dut.RawAPIs().GRIBI().Default(t)

	// Configure the gRIBI client clientA with election ID of 10.
	clientA := fluent.NewClient()
	clientA.Connection().WithStub(gribic).
		WithPersistence().
		WithRedundancyMode(fluent.ElectedPrimaryClient).
		WithInitialElectionID(clientAOriginElectionID /* low */, 0 /* hi */) // ID must be > 0.

	clientB := fluent.NewClient()
	clientB.Connection().WithStub(gribic).
		WithPersistence().
		WithInitialElectionID(clientBOriginElectionID, 0).
		WithRedundancyMode(fluent.ElectedPrimaryClient)

	clientA.Start(ctx, t)
	defer clientA.Stop(t)
	clientA.StartSending(ctx, t)
	if err := awaitTimeout(ctx, clientA, t, time.Minute); err != nil {
		t.Fatalf("Await got error during session negotiation: %v", err)
	}

	clientB.Start(ctx, t)
	defer clientB.Stop(t)
	clientB.StartSending(ctx, t)
	if err := awaitTimeout(ctx, clientB, t, time.Minute); err != nil {
		t.Fatalf("Await got error during session negotiation: %v", err)
	}

	testFlushWithDefaultNetworkInstance(ctx, t, clientA, clientB, ate, ateTop)

}

// testFlushWithDefaultNetWorkInstance tests flush with default network instance
func testFlushWithDefaultNetworkInstance(ctx context.Context, t *testing.T, clientA, clientB *fluent.GRIBIClient, ate *ondatra.ATEDevice, ateTop *ondatra.ATETopology) {
	// Inject an entry into the default network instance pointing to ATE port-2.
	// clientA is primary client
	injectEntry(ctx, t, clientA, defaultNetworkInstance)
	srcEndPoint := ateTop.Interfaces()[atePort1.Name]
	dstEndPoint := ateTop.Interfaces()[atePort2.Name]
	// Test traffic between ATE port-1 and ATE port-2.
	flowPath := testTraffic(t, ate, ateTop, srcEndPoint, dstEndPoint)
	if got := flowPath.LossPct().Get(t); got > 0 {
		t.Errorf("LossPct for flow got %g, want 0", got)
	} else {
		t.Log("Traffic can be forwarded between ATE port-1 and ATE port-2")
	}

	_, err := flush(ctx, t, clientA, clientAOriginElectionID, defaultNetworkInstance)
	if err != nil {
		t.Errorf("Unexpected error from flush, got: %v", err)
	}
	// After flush, left entry should be 0, and packets can no longer be forwarded.
	flowPath = testTraffic(t, ate, ateTop, srcEndPoint, dstEndPoint)
	if got := flowPath.LossPct().Get(t); got == 0 {
		t.Error("Traffic can still be forwarded between ATE port-1 and ATE port-2")
	} else {
		t.Log("Traffic can not be forwarded between ATE port-1 and ATE port-2")
	}
	leftEntries := checkNIHasNEntries(ctx, clientA, defaultNetworkInstance, t)
	if leftEntries != 0 {
		t.Errorf("Network instance has %d entry/entries, wanted: %d", leftEntries, 0)
	}

	// clientA is primary client
	injectEntry(ctx, t, clientA, defaultNetworkInstance)

	// flush should be failed, and remains 3 entries.
	flushRes, err := flush(ctx, t, clientB, clientBOriginElectionID, defaultNetworkInstance)
	if err == nil {
		t.Errorf("Flush should return an error, got response: %v", flushRes)
	}
	leftEntries = checkNIHasNEntries(ctx, clientB, defaultNetworkInstance, t)
	if leftEntries != 3 {
		t.Errorf("Network instance has %d entry/entries, wanted: %d", leftEntries, 3)
	}

	// Increases clientB's election ID to makes it be the primary client.
	clientB.Modify().UpdateElectionID(t, clientBUpdatedElectionID, 0)

	// Flush should be succeed and 0 entry left.
	_, err = flush(ctx, t, clientB, clientBUpdatedElectionID, defaultNetworkInstance)
	if err != nil {
		t.Fatalf("Unexpected error from flush, got: %v", err)
	}
	leftEntries = checkNIHasNEntries(ctx, clientB, defaultNetworkInstance, t)
	if leftEntries != 0 {
		t.Errorf("Network instance has %d entry/entries, wanted: %d", leftEntries, 0)
	}
}

// configureDUT configures port1-2 on the DUT.
func configureDUT(t *testing.T, dut *ondatra.DUTDevice) {
	t.Helper()
	d := dut.Config()

	p1 := dut.Port(t, "port1")
	p2 := dut.Port(t, "port2")

	d.Interface(p1.Name()).Replace(t, dutPort1.NewInterface(p1.Name()))
	d.Interface(p2.Name()).Replace(t, dutPort2.NewInterface(p2.Name()))

}

// configureATE configures port1, port2 on the ATE.
func configureATE(t *testing.T, ate *ondatra.ATEDevice) *ondatra.ATETopology {
	t.Helper()
	top := ate.Topology().New()

	p1 := ate.Port(t, "port1")
	i1 := top.AddInterface(atePort1.Name).WithPort(p1)
	i1.IPv4().
		WithAddress(atePort1.IPv4CIDR()).
		WithDefaultGateway(dutPort1.IPv4)

	p2 := ate.Port(t, "port2")
	i2 := top.AddInterface(atePort2.Name).WithPort(p2)
	i2.IPv4().
		WithAddress(atePort2.IPv4CIDR()).
		WithDefaultGateway(dutPort2.IPv4)

	return top
}

// awaitTimeout calls a fluent client Await, adding a timeout to the context.
func awaitTimeout(ctx context.Context, c *fluent.GRIBIClient, t testing.TB, timeout time.Duration) error {
	subctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.Await(subctx, t)
}

// injectEntry adds a fully referenced IPv4Entry.
func injectEntry(ctx context.Context, t *testing.T, client *fluent.GRIBIClient, networkInstanceName string) {
	t.Helper()
	client.Modify().AddEntry(t,
		fluent.NextHopEntry().
			WithNetworkInstance(networkInstanceName).
			WithIndex(1).
			WithIPAddress("192.0.2.6"),
		fluent.NextHopGroupEntry().
			WithNetworkInstance(networkInstanceName).
			WithID(42).
			AddNextHop(1, 1),
		fluent.IPv4Entry().
			WithNetworkInstance(networkInstanceName).
			WithPrefix(ateDstNetCIDR).
			WithNextHopGroupNetworkInstance(networkInstanceName).
			WithNextHopGroup(42),
	)

	if err := awaitTimeout(ctx, client, t, time.Minute); err != nil {
		t.Fatalf("Unexpected error from server - entries, got: %v, want: nil", err)
	}
	res := client.Results(t)

	// Check the three entries in order.
	chk.HasResult(t, res,
		fluent.OperationResult().
			WithOperationID(1).
			WithProgrammingResult(fluent.InstalledInRIB).
			AsResult(),
	)
	chk.HasResult(t, res,
		fluent.OperationResult().
			WithOperationID(2).
			WithProgrammingResult(fluent.InstalledInRIB).
			AsResult(),
	)
	chk.HasResult(t, res,
		fluent.OperationResult().
			WithOperationID(3).
			WithProgrammingResult(fluent.InstalledInRIB).
			AsResult(),
	)
}

// testTraffic generates traffic flow from source network to
// destination network via srcEndPoint to dstEndPoint and checks for
// packet loss.
func testTraffic(t *testing.T, ate *ondatra.ATEDevice, top *ondatra.ATETopology, srcEndPoint, dstEndPoint *ondatra.Interface) *ateflow.FlowPath {
	// Ensure that traffic can be forwarded between ATE port-1 and ATE port-2.
	t.Helper()
	ethHeader := ondatra.NewEthernetHeader()
	ipv4Header := ondatra.NewIPv4Header()
	ipv4Header.DstAddressRange().
		WithMin("198.51.100.0").
		WithMax("198.51.100.254").
		WithCount(250)

	flow := ate.Traffic().NewFlow("Flow").
		WithSrcEndpoints(srcEndPoint).
		WithDstEndpoints(dstEndPoint).
		WithHeaders(ethHeader, ipv4Header)

	ate.Traffic().Start(t, flow)
	time.Sleep(15 * time.Second)
	ate.Traffic().Stop(t)

	time.Sleep(time.Minute)

	flowPath := ate.Telemetry().Flow(flow.Name())
	return flowPath
}

// flush flushes all the state on the server, but does not validate it specifically.
func flush(ctx context.Context, t *testing.T, client *fluent.GRIBIClient, electionID uint64, networkInstanceName string) (*gpb.FlushResponse, error) {
	t.Helper()
	res, err := client.Flush().
		WithElectionID(electionID, 0).
		WithNetworkInstance(networkInstanceName).
		Send()
	return res, err
}

// checkNIHasNEntries uses the Get RPC to validate that the network instance named ni
// contains want (an integer) entries.
func checkNIHasNEntries(ctx context.Context, c *fluent.GRIBIClient, ni string, t testing.TB) int {
	t.Helper()
	gr, err := c.Get().
		WithNetworkInstance(ni).
		WithAFT(fluent.AllAFTs).
		Send()

	if err != nil {
		t.Fatalf("Unexpected error from get, got: %v", err)
	}
	return len(gr.GetEntry())
}
