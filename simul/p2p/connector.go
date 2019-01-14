package main

import (
	"fmt"
	"math/rand"
)

// Connector holds the logic to connect a node to a set of IDs on the overlay
// network
type Connector interface {
	Connect(node *P2PNode, ids []*P2PIdentity, max int) error
}

type neighbor struct{}

// NewNeighborConnector returns a connector that connects to its most immediate
// neighbors - ids.
func NewNeighborConnector() Connector {
	return &neighbor{}
}

func (*neighbor) Connect(node *P2PNode, ids []*P2PIdentity, max int) error {
	nodeID := int(node.handelID)
	baseID := nodeID
	n := len(ids)
	firstLoop := false
	for chosen := 0; chosen < max; chosen++ {
		if baseID == n {
			if firstLoop {
				fmt.Println("neighbor connection is looping!")
				panic("aie")
			}
			baseID = 0
			firstLoop = true
		}
		if baseID == nodeID {
			baseID++
			continue
		}
		if err := node.Connect(ids[baseID]); err != nil {
			return err
		}
		//fmt.Printf("node %d connected to %d\n", nodeID, baseID)
		baseID++
	}
	return nil
}

type random struct{}

// NewRandomConnector returns a Connector that connects nodes randomly
func NewRandomConnector() Connector { return &random{} }

func (*random) Connect(node *P2PNode, ids []*P2PIdentity, max int) error {
	n := len(ids)
	own := node.handelID
	//fmt.Printf("- node %d connects to...", node.handelID)
	for chosen := 0; chosen < max; chosen++ {
		identity := ids[rand.Intn(n)]
		if identity.Identity.ID() == own {
			chosen--
			continue
		}

		if err := node.Connect(identity); err != nil {
			return err
		}
		//fmt.Printf(" %d -", identity.Identity.ID())
	}
	//fmt.Printf("\n")
	return nil
}