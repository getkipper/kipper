package installer

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServerTunnelAddressReadsTheFlannelLink(t *testing.T) {
	wg := &scriptedRunner{out: "5: flannel-wg    inet 10.42.0.0/32 scope global flannel-wg\\       valid_lft forever preferred_lft forever\n"}
	addr, err := ServerTunnelAddress(wg)
	require.NoError(t, err)
	assert.Equal(t, "10.42.0.0", addr)
	assert.Contains(t, wg.commands[0], "flannel-wg")
	assert.Contains(t, wg.commands[0], "flannel.1", "a cluster on the vxlan backend has flannel.1 instead")

	for _, out := range []string{"", "Device \"flannel-wg\" does not exist.\n", "5: flannel-wg    inet not-an-ip/32 scope global\n"} {
		_, err := ServerTunnelAddress(&scriptedRunner{out: out})
		assert.Error(t, err, out)
	}
	_, err = ServerTunnelAddress(&scriptedRunner{err: errors.New("ssh: connection lost")})
	assert.Error(t, err)
}

func TestTunnelRTTCommandRefusesANonAddress(t *testing.T) {
	for _, addr := range []string{"", "server-1", "10.42.0.0; reboot", "$(id)"} {
		_, err := TunnelRTTCommand(addr)
		assert.Error(t, err, addr)
	}
	cmd, err := TunnelRTTCommand("10.42.0.0")
	require.NoError(t, err)
	assert.Contains(t, cmd, "ping -n -c 20")
	assert.Contains(t, cmd, "10.42.0.0")
	assert.True(t, strings.HasPrefix(cmd, "timeout "))
}

func pingOutput(times []string, received int) string {
	var b strings.Builder
	b.WriteString("PING 10.42.0.0 (10.42.0.0) 56(84) bytes of data.\n")
	for i, ms := range times {
		fmt.Fprintf(&b, "64 bytes from 10.42.0.0: icmp_seq=%d ttl=64 time=%s ms\n", i+1, ms)
	}
	b.WriteString("\n--- 10.42.0.0 ping statistics ---\n")
	fmt.Fprintf(&b, "20 packets transmitted, %d received, %d%% packet loss, time 3805ms\n", received, 100*(20-received)/20)
	b.WriteString("rtt min/avg/max/mdev = 0.410/0.600/4.200/0.800 ms\n")
	return b.String()
}

func TestParsePing(t *testing.T) {
	times := make([]string, 0, 20)
	for i := 0; i < 18; i++ {
		times = append(times, "0.410")
	}
	times = append(times, "1.25", "4.20")
	s, err := ParsePing(pingOutput(times, 20))
	require.NoError(t, err)
	assert.Equal(t, 20, s.Sent)
	assert.Zero(t, s.Lost)
	assert.InDelta(t, 4.2, s.P99Ms, 1e-9, "the 99th percentile of 20 samples is the slowest")
	assert.InDelta(t, 4.2, s.MaxMs, 1e-9)

	s, err = ParsePing(pingOutput([]string{"0.5", "0.6"}, 2))
	require.NoError(t, err)
	assert.Equal(t, 20, s.Sent)
	assert.Equal(t, 18, s.Lost)

	s, err = ParsePing("PING 10.42.0.0 (10.42.0.0) 56(84) bytes of data.\n64 bytes from 10.42.0.0: icmp_seq=1 ttl=64 time=0.5 ms\n")
	require.NoError(t, err, "replies before the timeout cut ping off are still a measurement")
	assert.Equal(t, tunnelPings, s.Sent)
	assert.Equal(t, tunnelPings-1, s.Lost)

	busybox := "64 bytes from 10.42.0.0: seq=0 ttl=64 time=0.512 ms\n\n--- 10.42.0.0 ping statistics ---\n20 packets transmitted, 1 packets received, 95% packet loss\n"
	s, err = ParsePing(busybox)
	require.NoError(t, err)
	assert.Equal(t, 19, s.Lost)

	s, err = ParsePing(pingOutput(nil, 0))
	require.NoError(t, err, "a tunnel that answers nothing is a measurement: all pings lost")
	assert.Equal(t, 20, s.Lost)

	_, err = ParsePing("bash: ping: command not found\n")
	assert.Error(t, err)
}

func TestMeasureTunnelRTTKeepsAPartialMeasurement(t *testing.T) {
	s, err := MeasureTunnelRTT(&scriptedRunner{out: pingOutput([]string{"0.5"}, 1), err: errors.New("exit status 1")}, "10.42.0.0")
	require.NoError(t, err, "ping exits non-zero when a reply is missing")
	assert.Equal(t, 19, s.Lost)

	_, err = MeasureTunnelRTT(&scriptedRunner{out: "ping: command not found\n", err: errors.New("exit status 127")}, "10.42.0.0")
	assert.Error(t, err)
}
