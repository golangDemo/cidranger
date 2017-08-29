/*
Package trie provides an LPC (Level Path Compressed) trie implementation of the
ranger interface inspired by this blog post:
https://vincent.bernat.im/en/blog/2017-ipv4-route-lookup-linux

CIDR blocks are stored using a prefix tree structure where each node has its
parent as prefix, and the path from the root node represents current CIDR block.

For IPv4, the trie structure guarantees max depth of 32 as IPv4 addresses are
32 bits long and each bit represents a prefix tree starting at that bit. This
property also gaurantees constant lookup time in Big-O notation.

Path compression compresses a string of node with only 1 child into a single
node, decrease the amount of lookups necessary during containment tests.

Level compression dictates the amount of direct children of a node by allowing
it to handle multiple bits in the path.  The heuristic (based on children
population) to decide when the compression and decompression happens is outlined
in the prior linked blog, and will be experimented with in more depth in this
project in the future.

TODO: Implement level-compressed component of the LPC trie.
TODO: Add support for ipV6.

*/
package trie

import (
	"fmt"
	"net"
	"strings"

	rnet "github.com/yl2chen/cidranger/net"
	"github.com/yl2chen/cidranger/ranger"
)

// PrefixTrie is a level-path-compressed (LPC) trie for cidr ranges.
// TODO: Implement level-compressed capability
type PrefixTrie struct {
	parent   *PrefixTrie
	children []*PrefixTrie

	numBitsSkipped uint
	numBitsHandled uint

	network  rnet.Network
	hasEntry bool
}

// NewPrefixTree creates a new PrefixTrie.
func NewPrefixTree() *PrefixTrie {
	_, rootNet, _ := net.ParseCIDR("0.0.0.0/0")

	return &PrefixTrie{
		children:       make([]*PrefixTrie, 2, 2),
		numBitsSkipped: 0,
		numBitsHandled: 1,
		network:        rnet.NewNetwork(*rootNet),
	}
}

func newPathPrefixTrie(network rnet.Network, numBitsSkipped uint) (*PrefixTrie, error) {
	path := NewPrefixTree()
	path.numBitsSkipped = numBitsSkipped
	path.network = network.Masked(int(numBitsSkipped))
	return path, nil
}

func newEntryTrie(network rnet.Network) (*PrefixTrie, error) {
	ones, _ := network.IPNet.Mask.Size()
	leaf, err := newPathPrefixTrie(network, uint(ones))
	if err != nil {
		return nil, err
	}
	leaf.hasEntry = true
	return leaf, nil
}

// Insert inserts the given cidr range into prefix trie.
func (p *PrefixTrie) Insert(network net.IPNet) error {
	return p.insert(rnet.NewNetwork(network))
}

// Remove removes network from trie.
func (p *PrefixTrie) Remove(network net.IPNet) (*net.IPNet, error) {
	return p.remove(rnet.NewNetwork(network))
}

// Contains returns boolean indicating whether given ip is contained in any
// of the inserted networks.
func (p *PrefixTrie) Contains(ip net.IP) (bool, error) {
	nn := rnet.NewNetworkNumber(ip)
	if nn == nil {
		return false, ranger.ErrInvalidNetworkNumberInput
	}
	return p.contains(nn)
}

// ContainingNetworks returns the list of networks given ip is a part of in
// ascending prefix order.
func (p *PrefixTrie) ContainingNetworks(ip net.IP) ([]net.IPNet, error) {
	nn := rnet.NewNetworkNumber(ip)
	if nn == nil {
		return nil, ranger.ErrInvalidNetworkNumberInput
	}
	return p.containingNetworks(nn)
}

// String returns string representation of trie, mainly for visualization and
// debugging.
func (p *PrefixTrie) String() string {
	children := []string{}
	padding := strings.Repeat("| ", p.level()+1)
	for bits, child := range p.children {
		if child == nil {
			continue
		}
		childStr := fmt.Sprintf("\n%s%d--> %s", padding, bits, child.String())
		children = append(children, childStr)
	}
	return fmt.Sprintf("%s (target_pos:%d:has_entry:%t)%s", p.network,
		p.targetBitPosition(), p.hasEntry, strings.Join(children, ""))
}

func (p *PrefixTrie) contains(number rnet.NetworkNumber) (bool, error) {
	if !p.network.Contains(number) {
		return false, nil
	}
	if p.hasEntry {
		return true, nil
	}
	bit, err := p.targetBitFromIP(number)
	if err != nil {
		return false, err
	}
	child := p.children[bit]
	if child != nil {
		return child.contains(number)
	}
	return false, nil
}

func (p *PrefixTrie) containingNetworks(number rnet.NetworkNumber) ([]net.IPNet, error) {
	results := []net.IPNet{}
	if !p.network.Contains(number) {
		return results, nil
	}
	if p.hasEntry {
		results = []net.IPNet{p.network.IPNet}
	}
	bit, err := p.targetBitFromIP(number)
	if err != nil {
		return nil, err
	}
	child := p.children[bit]
	if child != nil {
		ranges, err := child.containingNetworks(number)
		if err != nil {
			return nil, err
		}
		if len(ranges) > 0 {
			results = append(results, ranges...)
		}
	}
	return results, nil
}

func (p *PrefixTrie) insert(network rnet.Network) error {
	if p.network.Equal(network) {
		p.hasEntry = true
		return nil
	}
	bit, err := p.targetBitFromIP(network.Number)
	if err != nil {
		return err
	}
	child := p.children[bit]
	if child == nil {
		var entry *PrefixTrie
		entry, err = newEntryTrie(network)
		if err != nil {
			return err
		}
		return p.insertPrefix(bit, entry)
	}

	lcb, err := network.LeastCommonBitPosition(child.network)
	if err != nil {
		return err
	}
	if lcb-1 > child.targetBitPosition() {
		child, err = newPathPrefixTrie(network, 32-lcb)
		if err != nil {
			return err
		}
		err := p.insertPrefix(bit, child)
		if err != nil {
			return err
		}
	}
	return child.insert(network)
}

func (p *PrefixTrie) insertPrefix(bits uint32, prefix *PrefixTrie) error {
	child := p.children[bits]
	if child != nil {
		prefixBit, err := prefix.targetBitFromIP(child.network.Number)
		if err != nil {
			return err
		}
		prefix.insertPrefix(prefixBit, child)
	}
	p.children[bits] = prefix
	prefix.parent = p
	return nil
}

func (p *PrefixTrie) remove(network rnet.Network) (*net.IPNet, error) {
	if p.hasEntry && p.network.Equal(network) {
		if p.childrenCount() > 1 {
			p.hasEntry = false
		} else {
			// Has 0 or 1 child.
			parentBits, err := p.parent.targetBitFromIP(network.Number)
			if err != nil {
				return nil, err
			}
			var skipChild *PrefixTrie
			for _, child := range p.children {
				if child != nil {
					skipChild = child
					break
				}
			}
			p.parent.children[parentBits] = skipChild
		}
		return &network.IPNet, nil
	}
	bit, err := p.targetBitFromIP(network.Number)
	if err != nil {
		return nil, err
	}
	child := p.children[bit]
	if child != nil {
		return child.remove(network)
	}
	return nil, nil
}

func (p *PrefixTrie) childrenCount() int {
	count := 0
	for _, child := range p.children {
		if child != nil {
			count++
		}
	}
	return count
}

func (p *PrefixTrie) targetBitPosition() uint {
	return 31 - p.numBitsSkipped
}

func (p *PrefixTrie) targetBitFromIP(n rnet.NetworkNumber) (uint32, error) {
	return n.Bit(p.targetBitPosition())
}

func (p *PrefixTrie) level() int {
	if p.parent == nil {
		return 0
	}
	return p.parent.level() + 1
}

// walkDepth walks the trie in depth order, for unit testing.
func (p *PrefixTrie) walkDepth() <-chan net.IPNet {
	networks := make(chan net.IPNet)
	go func() {
		if p.hasEntry {
			networks <- p.network.IPNet
		}
		subNetworks := []<-chan net.IPNet{}
		for _, trie := range p.children {
			if trie == nil {
				continue
			}
			subNetworks = append(subNetworks, trie.walkDepth())
		}
		for _, subNetwork := range subNetworks {
			for network := range subNetwork {
				networks <- network
			}
		}
		close(networks)
	}()
	return networks
}
