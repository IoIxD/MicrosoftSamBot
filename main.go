package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"

	"github.com/diamondburned/arikawa/v3/api"
	"github.com/diamondburned/arikawa/v3/gateway"
	"github.com/diamondburned/arikawa/v3/state"
	"github.com/diamondburned/arikawa/v3/voice"
	"github.com/diamondburned/oggreader"
	"github.com/naoina/toml"
)

var s *state.State // i dun give a fuck

var commands = []api.CreateCommandData{
	{
		Name:        "say",
		Description: "say something in the VC you're in",
	},
}

var Config struct {
	Token string
}

var textFlag = flag.String("text", "", "text for bot to say")
var configPath = flag.String("config", "config.toml", "path to config.toml")
var outputPath = flag.String("output", "audio.wav", "file to save the audio too if this is used as a standalone program")

func main() {
	flag.Parse()

	// config shit
	file, err := os.ReadFile(*configPath)
	if err != nil {
		fmt.Println("Error while reading config: ", err)
		return
	}
	if err := toml.Unmarshal(file, &Config); err != nil {
		fmt.Println("Error while parsing config: ", err)
		return
	}

	text := strings.ReplaceAll(*textFlag, " ", "%20")
	// if it's blank, run the discord bot
	if text == "" {
		err = DiscordThread()
		if err != nil {
			fmt.Println(err)
			return
		}
	} else {
		b, err := SamSay(text)
		if err != nil {
			fmt.Println(err)
			return
		}

		fuck, err := os.Create(*outputPath)
		if err != nil {
			fmt.Println(err)
			return
		}

		fuck.Write(b)
	}
}

func SamSay(text string) ([]byte, error) {
	url := "https://tetyys.com/SAPI4/SAPI4?text=" + text + "&voice=Sam&pitch=100&speed=100"

	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}

	return io.ReadAll(resp.Body)
}

func DiscordThread() (err error) {
	ctx, done := signal.NotifyContext(context.Background(), os.Interrupt)
	defer done()

	s = state.New("Bot " + Config.Token)
	s.AddIntents(0 |
		gateway.IntentGuildMessages |
		gateway.IntentGuilds |
		gateway.IntentGuildVoiceStates,
	//gateway.IntentGuildMembers,
	)
	if err = s.Open(ctx); err != nil {
		return
	}
	self, err := s.Me()
	if err != nil {
		return
	}
	log.Printf("Connected to Discord as %s#%s (%s)\n", self.Username, self.Discriminator, self.ID)

	s.AddHandler(func(e *gateway.MessageCreateEvent) {
		MessageHandler(e, ctx)
	})

	select {
	case <-ctx.Done():
		done()
	}

	return nil
}

func MessageHandler(e *gateway.MessageCreateEvent, ctx context.Context) {
	message := e.Message

	if message.Content[0] != ';' {
		return
	}

	// process the audio
	text := strings.Replace(e.Message.Content, ";", "", 1)
	content, err := SamSay(text)
	if err != nil {
		s.Client.SendTextReply(e.ChannelID, err.Error(), message.ID)
		return
	}

	// convert it to ogg (relunctantly)
	v, err := voice.NewSession(s)
	if err != nil {
		s.Client.SendTextReply(e.ChannelID, err.Error(), message.ID)
		return
	}

	ffmpeg := exec.CommandContext(ctx,
		"ffmpeg", "-hide_banner", "-loglevel", "error",
		// "one thread is all we need" according to the
		// example. TODO: is it really?
		"-threads", "1",
		// input
		"-i", "-",
		// conversion to ogg.
		"-acodec", "libvorbis",
		// Bitrate in kilobits. This doesn't matter, but I recommend 96k as the
		// sweet spot.
		"-bit_rate", "96k",
		// Frame duration should be the same as what's given into
		// udp.DialFuncWithFrequency.
		"-frame_duration", strconv.Itoa(60),
		// Disable variable bitrate to keep packet sizes consistent. This is
		// optional.
		"-vbr", "off",
		// Output format, which is opus, so we need to unwrap the opus file.
		"-f", "opus",
		"-",
	)

	ffmpeg.Stderr = os.Stderr
	ffmpeg.Stdin = bytes.NewReader(content)

	stdout, err := ffmpeg.StdoutPipe()
	if err != nil {
		s.Client.SendTextReply(e.ChannelID, "failed to get stdout pipe: "+err.Error(), message.ID)
		return
	}

	// Kickstart FFmpeg before we join. FFmpeg will wait until we start
	// consuming the stream to process further.
	if err := ffmpeg.Start(); err != nil {
		s.Client.SendTextReply(e.ChannelID, "failed to start ffmpeg: "+err.Error(), message.ID)
		return
	}

	// go through the guild the user was in and find what channel they are in.
	ch, err := s.Cabinet.VoiceState(e.GuildID, message.Author.ID)

	// Join the voice channel.
	if err := v.JoinChannelAndSpeak(ctx, ch.ChannelID, false, true); err != nil {
		s.Client.SendTextReply(e.ChannelID, "failed to join channel: "+err.Error(), message.ID)
		return
	}
	defer v.Leave(ctx)

	// Start decoding FFmpeg's OGG-container output and extract the raw Opus
	// frames into the stream.
	if err := oggreader.DecodeBuffered(v, stdout); err != nil {
		s.Client.SendTextReply(e.ChannelID, "failed to decode wav: "+err.Error(), message.ID)
		return
	}

	// Wait until FFmpeg finishes writing entirely and leave.
	if err := ffmpeg.Wait(); err != nil {
		s.Client.SendTextReply(e.ChannelID, "failed to finish ffmpeg: "+err.Error(), message.ID)
		return
	}

	return
}
