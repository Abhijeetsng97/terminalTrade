package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/Abhijeetsng97/terminalTrade/internal/config"
	"github.com/Abhijeetsng97/terminalTrade/internal/store"
)

func main() {
	cfg, _ := config.Load()
	ctx := context.Background()
	st, err := store.Connect(ctx, cfg.MongoURI, cfg.MongoDB, cfg.EncryptionKey)
	if err != nil { fmt.Println("store:", err); os.Exit(1) }
	defer st.Close(ctx)
	raw, err := st.LoadCredentials(ctx, "KITE")
	if err != nil { fmt.Println("no creds:", err); os.Exit(1) }
	var cb struct{ AccessToken string `json:"access_token"` }
	_ = json.Unmarshal(raw, &cb)
	fmt.Print(cb.AccessToken)
}
