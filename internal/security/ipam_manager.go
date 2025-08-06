package security

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// Subnet represents a network subnet
type Subnet struct {
	CIDR      string    `json:"cidr"`
	Gateway   string    `json:"gateway"`
	NodeID    string    `json:"node_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	IP        net.IP    `json:"ip"`
	MaskBits  int       `json:"mask_bits"`
}

// IPAllocation represents an IP address allocation
type IPAllocation struct {
	IP          string    `json:"ip"`
	SubnetID    string    `json:"subnet_id"`
	ContainerID string    `json:"container_id"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// IPAMManager handles IP address management
type IPAMManager struct {
	logger       *logrus.Logger
	networkCIDR  string
	subnetMask   int
	subnets      map[string]*Subnet
	allocations  map[string]*IPAllocation
	subnetsMutex sync.RWMutex
	allocMutex   sync.RWMutex
}

// NewIPAMManager creates a new IPAM manager
func NewIPAMManager(logger *logrus.Logger) *IPAMManager {
	return &IPAMManager{
		logger:      logger,
		subnets:     make(map[string]*Subnet),
		allocations: make(map[string]*IPAllocation),
	}
}

// Initialize initializes the IPAM manager with a network CIDR
func (m *IPAMManager) Initialize(ctx context.Context, networkCIDR string, subnetMask int) error {
	// Validate network CIDR
	_, ipNet, err := net.ParseCIDR(networkCIDR)
	if err != nil {
		return fmt.Errorf("invalid network CIDR: %w", err)
	}

	// Validate subnet mask
	networkMask, _ := ipNet.Mask.Size()
	if subnetMask <= networkMask {
		return fmt.Errorf("subnet mask /%d must be larger than network mask /%d", subnetMask, networkMask)
	}

	if subnetMask > 30 {
		return fmt.Errorf("subnet mask /%d is too large, maximum allowed is /30", subnetMask)
	}

	m.networkCIDR = networkCIDR
	m.subnetMask = subnetMask

	m.logger.Infof("IPAM initialized with network CIDR %s and subnet mask /%d", networkCIDR, subnetMask)
	return nil
}

// GetNodeSubnet retrieves the subnet allocated to a node
func (m *IPAMManager) GetNodeSubnet(ctx context.Context, nodeID string) (*Subnet, error) {
	m.subnetsMutex.RLock()
	defer m.subnetsMutex.RUnlock()

	// Find subnet for the specified node
	for _, subnet := range m.subnets {
		if subnet.NodeID == nodeID {
			return subnet, nil
		}
	}

	return nil, fmt.Errorf("no subnet found for node %s", nodeID)
}

// AllocateNodeSubnet allocates a subnet for a node
func (m *IPAMManager) AllocateNodeSubnet(ctx context.Context, nodeID string) (*Subnet, error) {
	if nodeID == "" {
		return nil, fmt.Errorf("nodeID cannot be empty")
	}

	m.subnetsMutex.Lock()
	defer m.subnetsMutex.Unlock()

	// Check if node already has a subnet
	for _, subnet := range m.subnets {
		if subnet.NodeID == nodeID {
			return subnet, nil
		}
	}

	// Parse network CIDR
	_, ipNet, err := net.ParseCIDR(m.networkCIDR)
	if err != nil {
		return nil, fmt.Errorf("invalid network CIDR: %w", err)
	}

	// Find an available subnet
	baseIP := ipNet.IP.To4()
	if baseIP == nil {
		return nil, fmt.Errorf("IPv6 not supported")
	}

	// Calculate number of subnets
	networkMask, _ := ipNet.Mask.Size()
	if networkMask >= m.subnetMask {
		return nil, fmt.Errorf("subnet mask /%d must be larger than network mask /%d", m.subnetMask, networkMask)
	}

	numSubnets := 1 << uint(m.subnetMask-networkMask)

	// Try to allocate a subnet
	for i := 0; i < numSubnets; i++ {
		// Calculate subnet CIDR
		subnetIP := make(net.IP, len(baseIP))
		copy(subnetIP, baseIP)

		// Set subnet bits
		for j := 0; j < m.subnetMask-networkMask; j++ {
			byteIndex := networkMask/8 + j/8
			bitIndex := 7 - ((networkMask + j) % 8)

			if byteIndex >= len(subnetIP) {
				continue // Skip if we're out of bounds
			}

			if (i & (1 << uint(j))) != 0 {
				subnetIP[byteIndex] |= 1 << uint(bitIndex)
			}
		}

		subnetCIDR := fmt.Sprintf("%s/%d", subnetIP.String(), m.subnetMask)

		// Check if subnet is already allocated
		if _, exists := m.subnets[subnetCIDR]; exists {
			continue
		}

		// Calculate gateway (first IP in subnet)
		_, subnetIPNet, err := net.ParseCIDR(subnetCIDR)
		if err != nil {
			m.logger.Warnf("Failed to parse subnet CIDR %s: %v", subnetCIDR, err)
			continue
		}

		gateway := make(net.IP, len(subnetIP))
		copy(gateway, subnetIPNet.IP)
		gateway[3]++

		// Create subnet
		subnet := &Subnet{
			CIDR:      subnetCIDR,
			Gateway:   gateway.String(),
			NodeID:    nodeID,
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
			IP:        subnetIPNet.IP,
			MaskBits:  m.subnetMask,
		}

		m.subnets[subnetCIDR] = subnet
		m.logger.Infof("Allocated subnet %s for node %s", subnetCIDR, nodeID)

		return subnet, nil
	}

	return nil, fmt.Errorf("no available subnets")
}

// ReleaseNodeSubnet releases a subnet allocated to a node
func (m *IPAMManager) ReleaseNodeSubnet(ctx context.Context, nodeID string) error {
	m.subnetsMutex.Lock()
	defer m.subnetsMutex.Unlock()

	// Find subnet for node
	var subnetCIDR string
	for cidr, subnet := range m.subnets {
		if subnet.NodeID == nodeID {
			subnetCIDR = cidr
			break
		}
	}

	if subnetCIDR == "" {
		return fmt.Errorf("no subnet found for node %s", nodeID)
	}

	// Release subnet
	delete(m.subnets, subnetCIDR)
	m.logger.Infof("Released subnet %s for node %s", subnetCIDR, nodeID)

	return nil
}

// AllocateIP allocates an IP address for a container
func (m *IPAMManager) AllocateIP(ctx context.Context, subnetCIDR, containerID string) (*IPAllocation, error) {
	m.allocMutex.Lock()
	defer m.allocMutex.Unlock()

	// Find the subnet by CIDR or by IP address
	var foundSubnet *Subnet
	m.subnetsMutex.RLock()
	for _, subnet := range m.subnets {
		if subnet.CIDR == subnetCIDR {
			foundSubnet = subnet
			break
		}
	}
	m.subnetsMutex.RUnlock()

	// If not found by exact CIDR, try to find by IP address
	if foundSubnet == nil {
		// Try to parse as IP address
		ip := net.ParseIP(subnetCIDR)
		if ip != nil {
			// If it's an IP address, find the subnet that contains it
			m.subnetsMutex.RLock()
			for _, subnet := range m.subnets {
				_, ipNet, _ := net.ParseCIDR(subnet.CIDR)
				if ipNet.Contains(ip) {
					foundSubnet = subnet
					subnetCIDR = subnet.CIDR
					break
				}
			}
			m.subnetsMutex.RUnlock()
		}
	}

	// If still not found, try to parse as IP/mask
	if foundSubnet == nil {
		// Try to parse as IP/mask
		ip, _, err := net.ParseCIDR(subnetCIDR)
		if err == nil {
			// If it's a valid CIDR, find the subnet that contains it
			m.subnetsMutex.RLock()
			for _, subnet := range m.subnets {
				_, subnetIPNet, _ := net.ParseCIDR(subnet.CIDR)
				if subnetIPNet.Contains(ip) {
					foundSubnet = subnet
					subnetCIDR = subnet.CIDR
					break
				}
			}
			m.subnetsMutex.RUnlock()
		}
	}

	// If still not found, return error
	if foundSubnet == nil {
		return nil, fmt.Errorf("subnet %s not found", subnetCIDR)
	}

	// Parse subnet CIDR
	_, ipNet, err := net.ParseCIDR(subnetCIDR)
	if err != nil {
		return nil, fmt.Errorf("invalid subnet CIDR: %w", err)
	}

	// Calculate number of IPs in subnet
	ones, bits := ipNet.Mask.Size()
	numIPs := 1 << uint(bits-ones)

	// Reserve first IP for gateway and last IP for broadcast
	for i := 1; i < numIPs-1; i++ {
		// Calculate IP
		ip := make(net.IP, len(ipNet.IP))
		copy(ip, ipNet.IP)

		// Add offset to IP
		for j := 0; j < 4; j++ {
			ip[j] += byte((i >> uint(8*(3-j))) & 0xff)
		}

		ipStr := ip.String()

		// Check if IP is already allocated
		allocated := false
		for _, alloc := range m.allocations {
			if alloc.IP == ipStr && alloc.SubnetID == subnetCIDR {
				allocated = true
				break
			}
		}

		if !allocated {
			// Allocate IP
			allocation := &IPAllocation{
				IP:          ipStr,
				SubnetID:    subnetCIDR,
				ContainerID: containerID,
				CreatedAt:   time.Now(),
				ExpiresAt:   time.Now().Add(24 * time.Hour),
			}

			m.allocations[ipStr] = allocation
			m.logger.Infof("Allocated IP %s for container %s in subnet %s", ipStr, containerID, subnetCIDR)

			return allocation, nil
		}
	}

	return nil, fmt.Errorf("no available IPs in subnet %s", subnetCIDR)
}

// ReleaseIP releases an allocated IP address
func (m *IPAMManager) ReleaseIP(ctx context.Context, ip string) error {
	m.allocMutex.Lock()
	defer m.allocMutex.Unlock()

	// Check if IP is allocated
	if _, exists := m.allocations[ip]; !exists {
		return fmt.Errorf("IP %s not allocated", ip)
	}

	// Release IP
	delete(m.allocations, ip)
	m.logger.Infof("Released IP %s", ip)

	return nil
}

// GetSubnets returns all allocated subnets
func (m *IPAMManager) GetSubnets(ctx context.Context) ([]*Subnet, error) {
	m.subnetsMutex.RLock()
	defer m.subnetsMutex.RUnlock()

	subnets := make([]*Subnet, 0, len(m.subnets))
	for _, subnet := range m.subnets {
		subnets = append(subnets, subnet)
	}

	return subnets, nil
}

// GetAllocations returns all IP allocations
func (m *IPAMManager) GetAllocations(ctx context.Context) ([]*IPAllocation, error) {
	m.allocMutex.RLock()
	defer m.allocMutex.RUnlock()

	allocations := make([]*IPAllocation, 0, len(m.allocations))
	for _, alloc := range m.allocations {
		allocations = append(allocations, alloc)
	}

	return allocations, nil
}
