package discord

import (
	"testing"

	"github.com/codered/spore/internal/config"
)

func TestAdmitMessage(t *testing.T) {
	cfg := config.DiscordConfig{
		Enabled: true, Token: "t",
		GuildID: "G", ChannelIDs: []string{"C1", "C2"}, UserIDs: []string{"U"},
		AllowDMs: true,
	}
	a := NewAdmitter(cfg)

	cases := []struct {
		name string
		in   Inbound
		want bool
	}{
		{"allowlisted user in an allowlisted channel",
			Inbound{UserID: "U", GuildID: "G", ChannelID: "C1"}, true},
		{"a thread is admitted on its parent channel",
			Inbound{UserID: "U", GuildID: "G", ChannelID: "T9", ParentID: "C2"}, true},
		{"a stranger in an allowlisted channel",
			Inbound{UserID: "STRANGER", GuildID: "G", ChannelID: "C1"}, false},
		{"the allowlisted user in another channel",
			Inbound{UserID: "U", GuildID: "G", ChannelID: "C9"}, false},
		{"the allowlisted user in another guild",
			Inbound{UserID: "U", GuildID: "OTHER", ChannelID: "C1"}, false},
		{"a thread whose parent is not allowlisted",
			Inbound{UserID: "U", GuildID: "G", ChannelID: "T9", ParentID: "C9"}, false},
		{"a DM from the allowlisted user",
			Inbound{UserID: "U", GuildID: "", ChannelID: "DM1"}, true},
		{"a DM from a stranger",
			Inbound{UserID: "STRANGER", GuildID: "", ChannelID: "DM2"}, false},
		// A bot echo is how a bridge talks to itself forever. The bot's own
		// messages come back over the gateway, so this is not hypothetical.
		{"a bot, even the allowlisted user id",
			Inbound{UserID: "U", Bot: true, GuildID: "G", ChannelID: "C1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.AdmitMessage(tc.in); got != tc.want {
				t.Fatalf("AdmitMessage(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestAdmitDMsOffByDefault(t *testing.T) {
	a := NewAdmitter(config.DiscordConfig{
		GuildID: "G", ChannelIDs: []string{"C1"}, UserIDs: []string{"U"}, AllowDMs: false,
	})
	if a.AdmitMessage(Inbound{UserID: "U", ChannelID: "DM1"}) {
		t.Fatal("a DM was admitted with allow_dms = false")
	}
}

func TestAdmitInteractionUsesTheSameRules(t *testing.T) {
	// The button press is a second entrance to the same house. If it were
	// checked differently from the message path, an attacker who could not
	// send a prompt could still answer an approval.
	cfg := config.DiscordConfig{
		GuildID: "G", ChannelIDs: []string{"C1"}, UserIDs: []string{"U"}, AllowDMs: true,
	}
	a := NewAdmitter(cfg)

	if !a.AdmitInteraction(Interaction{UserID: "U", GuildID: "G", ChannelID: "T1", ParentID: "C1"}) {
		t.Fatal("the allowlisted user's press in an allowlisted thread was rejected")
	}
	if a.AdmitInteraction(Interaction{UserID: "STRANGER", GuildID: "G", ChannelID: "T1", ParentID: "C1"}) {
		t.Fatal("a stranger's button press was admitted")
	}
	if a.AdmitInteraction(Interaction{UserID: "U", GuildID: "OTHER", ChannelID: "T1", ParentID: "C1"}) {
		t.Fatal("a press from another guild was admitted")
	}
}

func TestAdmitEmptyAllowlistAdmitsNobody(t *testing.T) {
	// config.Load rejects this shape, but the zero value must still fail
	// closed: a future caller that builds the struct directly gets the safe
	// behaviour, not an open door.
	a := NewAdmitter(config.DiscordConfig{})
	if a.AdmitMessage(Inbound{UserID: "U", ChannelID: "C1"}) {
		t.Fatal("the zero-value admitter admitted a message")
	}
	if a.AdmitInteraction(Interaction{UserID: "U", ChannelID: "C1"}) {
		t.Fatal("the zero-value admitter admitted an interaction")
	}
}
