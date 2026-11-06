package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"strconv"
	"syscall"

	"github.com/elee1766/acphost/pkg/acphost"
	"github.com/elee1766/acphost/pkg/hostenv"
	"github.com/elee1766/acphost/pkg/userns"
	"golang.org/x/sys/unix"
)

// userMaps returns the uid_map and gid_map lines newuidmap and
// newgidmap would get for u.
func userMaps(u userns.User) (*acphost.UserApplied, error) {
	self := os.Geteuid()
	subuid, err := userns.ReadSubIDs(hostenv.SubUIDPath, self)
	if err != nil {
		return nil, fmt.Errorf("user: %s: %w", hostenv.SubUIDPath, err)
	}
	subgid, err := userns.ReadSubIDs(hostenv.SubGIDPath, self)
	if err != nil {
		return nil, fmt.Errorf("user: %s: %w", hostenv.SubGIDPath, err)
	}
	ua, ga, err := userns.Maps(u, self, hostenv.SubUIDPath, hostenv.SubGIDPath, subuid, subgid)
	if err != nil {
		return nil, err
	}
	return &acphost.UserApplied{UIDMap: triples(ua), GIDMap: triples(ga)}, nil
}

func triples(args []string) []string {
	var l []string
	for i := 0; i+2 < len(args); i += 3 {
		l = append(l, args[i]+" "+args[i+1]+" "+args[i+2])
	}
	return l
}

// owner is "user:group" for st, numeric when the name is unknown.
func owner(st fs.FileInfo) string {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return "?"
	}
	u, g := strconv.Itoa(int(sys.Uid)), strconv.Itoa(int(sys.Gid))
	if x, err := user.LookupId(u); err == nil {
		u = x.Username
	}
	if x, err := user.LookupGroupId(g); err == nil {
		g = x.Name
	}
	return u + ":" + g
}

// signalNumber maps a signal name from proc.Exit back to its number.
func signalNumber(name string) int {
	if s := unix.SignalNum("SIG" + name); s != 0 {
		return int(s)
	}
	for i := 1; i < 65; i++ {
		if syscall.Signal(i).String() == name {
			return i
		}
	}
	return 0
}
