package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"time"
)

func main() {
	marker := time.Now().UnixNano()
	memory := make([]byte, 8<<20)
	if _, err := rand.Read(memory); err != nil { panic(err) }
	f, err := os.Create("/data/held-unlinked")
	if err != nil { panic(err) }
	if _, err = f.WriteString("tmpfs-preserved-open-fd"); err != nil { panic(err) }
	if _, err = f.Seek(6, 0); err != nil { panic(err) }
	if err = os.Remove("/data/held-unlinked"); err != nil { panic(err) }
	payload := make([]byte, 32<<20)
	if _, err = rand.Read(payload); err != nil { panic(err) }
	if err = os.WriteFile("/data/payload", payload, 0600); err != nil { panic(err) }
	for counter := 1; ; counter++ {
		data := make([]byte, 32)
		n, err := f.ReadAt(data, 0)
		if err != nil && n == 0 { panic(err) }
		offset, _ := f.Seek(0, 1)
		hash := sha256.Sum256(memory)
		state := map[string]any{"marker": marker, "counter": counter, "pid": os.Getpid(), "fd_text": string(data[:n]), "fd_offset": offset, "ram_sha256": hex.EncodeToString(hash[:]), "ram_bytes": len(memory)}
		body, _ := json.Marshal(state)
		if err = os.WriteFile("/probe-state.tmp", body, 0600); err != nil { panic(err) }
		if err = os.Rename("/probe-state.tmp", "/probe-state.json"); err != nil { panic(err) }
		time.Sleep(100 * time.Millisecond)
	}
}
