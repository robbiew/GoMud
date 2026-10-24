//go:build !windows

package connections

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"testing"

	"github.com/GoMudEngine/GoMud/internal/copyover"
)

func TestBBSAccountingAndCopyoverExclusion(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var peers []net.Conn
	add := func(kind ConnType) *ConnectionDetails {
		peer, e := net.Dial("tcp", listener.Addr().String())
		if e != nil {
			t.Fatal(e)
		}
		peers = append(peers, peer)
		c, e := listener.Accept()
		if e != nil {
			t.Fatal(e)
		}
		cd := Add(c, nil, kind)
		cd.SetState(LoggedIn)
		t.Cleanup(func() { _ = Remove(cd.ConnectionId()) })
		return cd
	}
	defer func() {
		for _, p := range peers {
			_ = p.Close()
		}
	}()
	base := ActiveBBSConnectionCount()
	legacyBase := ActiveNonBBSConnectionCount()
	human := add(ConnHuman)
	bbs := add(ConnBBS)
	if ActiveNonBBSConnectionCount() != legacyBase+1 {
		t.Fatal("BBS consumed the standard connection pool")
	}
	if ActiveBBSConnectionCount() != base+1 {
		t.Fatal("incorrect BBS pool accounting")
	}
	copyover.ResetRegistry()
	defer copyover.ResetRegistry()
	copyover.Register(CopyoverContributor())
	var buf bytes.Buffer
	if err := copyover.Save(&buf); err != nil {
		t.Fatal(err)
	}
	var count uint32
	_ = binary.Read(&buf, binary.BigEndian, &count)
	if count != 1 {
		t.Fatal("unexpected contributor count")
	}
	var nameSize uint16
	_ = binary.Read(&buf, binary.BigEndian, &nameSize)
	name := make([]byte, nameSize)
	_, _ = io.ReadFull(&buf, name)
	var size uint32
	_ = binary.Read(&buf, binary.BigEndian, &size)
	payload := make([]byte, size)
	_, _ = io.ReadFull(&buf, payload)
	var state connectionsState
	if err := json.Unmarshal(payload, &state); err != nil {
		t.Fatal(err)
	}
	foundHuman := false
	for _, rec := range state.Records {
		if rec.ConnectionId == bbs.ConnectionId() {
			t.Fatal("BBS TLS descriptor inherited as TCP")
		}
		if rec.ConnectionId == human.ConnectionId() {
			foundHuman = true
		}
	}
	if !foundHuman {
		t.Fatal("normal Telnet descriptor lost")
	}
}
