package installer

import (
	"bufio"
	"fmt"
	"math"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	tunnelPings          = 20
	tunnelTimeoutSeconds = 20
)

var (
	pingTimePattern        = regexp.MustCompile(`\btime=([0-9.]+) ms`)
	pingTransmittedPattern = regexp.MustCompile(`(\d+) packets transmitted`)
)

// ServerTunnelAddress reads the IPv4 address of the server's flannel link:
// flannel-wg on the WireGuard backend, flannel.1 on vxlan. Pod traffic
// between nodes, and with it Longhorn's replica traffic, crosses that link.
// It returns an error if the command fails or neither link has an IP address.
func ServerTunnelAddress(server CommandRunner) (string, error) {
	out, err := server.Run(`for link in flannel-wg flannel.1; do ip -4 -o addr show dev "$link" 2>/dev/null; done; true`)
	if err != nil {
		return "", fmt.Errorf("reading the server's pod network address: %w", err)
	}
	fields := strings.Fields(out)
	for i, f := range fields {
		if f != "inet" || i+1 == len(fields) {
			continue
		}
		addr, _, _ := strings.Cut(fields[i+1], "/")
		if ip := net.ParseIP(addr); ip != nil {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("the server has no flannel link with an address: %s", firstLine(out))
}

// TunnelRTTCommand returns a command that pings addr 20 times at 0.2 second
// intervals, with a 20 second overall timeout and a one second reply timeout.
// An invalid IP address returns an error.
func TunnelRTTCommand(addr string) (string, error) {
	ip := net.ParseIP(addr)
	if ip == nil {
		return "", fmt.Errorf("%q is not an IP address", addr)
	}
	return fmt.Sprintf("timeout %d ping -n -c %d -i 0.2 -W 1 %s", tunnelTimeoutSeconds, tunnelPings, ip.String()), nil
}

// ParsePing reads iputils or BusyBox ping output and returns the nearest-rank
// p99 and maximum reply time in milliseconds. Unanswered attempts, including
// those cut off by a timeout, count as lost out of at least 20. Output without
// a timed reply or a positive transmitted count returns an error.
func ParsePing(out string) (RTTSample, error) {
	var micros []int64
	sent := 0
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if m := pingTimePattern.FindStringSubmatch(line); m != nil {
			if ms, err := strconv.ParseFloat(m[1], 64); err == nil && ms >= 0 {
				micros = append(micros, int64(math.Round(ms*1000)))
			}
			continue
		}
		if m := pingTransmittedPattern.FindStringSubmatch(line); m != nil {
			sent, _ = strconv.Atoi(m[1])
		}
	}
	if len(micros) == 0 && sent == 0 {
		return RTTSample{}, fmt.Errorf("ping printed no measurement: %s", firstLine(out))
	}
	s := RTTSample{Sent: max(sent, tunnelPings, len(micros))}
	s.Lost = s.Sent - len(micros)
	if len(micros) > 0 {
		slices.Sort(micros)
		rank := int(math.Ceil(0.99*float64(len(micros)))) - 1
		s.P99Ms = float64(micros[rank]) / 1000
		s.MaxMs = float64(micros[len(micros)-1]) / 1000
	}
	return s, nil
}

// MeasureTunnelRTT pings the server's flannel address from the host behind client.
// It returns an error for an invalid address or output without a measurement.
// Partial measurements are accepted even when ping exits with an error.
func MeasureTunnelRTT(client CommandRunner, addr string) (RTTSample, error) {
	cmd, err := TunnelRTTCommand(addr)
	if err != nil {
		return RTTSample{}, err
	}
	out, runErr := client.Run(cmd)
	s, err := ParsePing(out)
	if err != nil {
		if runErr != nil {
			return RTTSample{}, fmt.Errorf("pinging %s over the pod network: %w", addr, runErr)
		}
		return RTTSample{}, err
	}
	return s, nil
}
