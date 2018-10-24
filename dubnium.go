package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"./sim"
)

type BotOutput struct {
	Pid						int
	Output					string
}

var bot_output_chan = make(chan BotOutput)		// Shared by all bot handlers.

// -----------------------------------------------------------------------------------------

func bot_handler(cmd string, pid int, io chan string, pregame string) {

	// There are 2 clear places where this handler can hang: the 2 Scan() calls.
	// Therefore it is essential that main() never try to send to the io channel
	// unless it knows that those scans succeeded.

	bot_is_kill := false

	cmd_split := strings.Fields(cmd)
	exec_command := exec.Command(cmd_split[0], cmd_split[1:]...)

	// Note that the command isn't run until we call Start().
	// So the following is just setup for that and shouldn't fail.

	i_pipe, _ := exec_command.StdinPipe()
	o_pipe, _ := exec_command.StdoutPipe()

	// No need to have a stderr pipe; without one it will go to /dev/null

	err := exec_command.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start bot %d (%s)\n", pid, cmd)
		bot_is_kill = true
	}

	if bot_is_kill == false {
		fmt.Fprintf(i_pipe, pregame)
		if pregame[len(pregame) - 1] != '\n' {
			fmt.Fprintf(i_pipe, "\n")
		}
	}

	scanner := bufio.NewScanner(o_pipe)

	if bot_is_kill == false && scanner.Scan() == false {				// So the Scan() happens if bot at least started.
		fmt.Fprintf(os.Stderr, "Bot %d output reached EOF\n", pid)
		bot_output_chan <- BotOutput{pid, "Non-starter (EOF)"}
		bot_is_kill = true
	} else if bot_is_kill {
		bot_output_chan <- BotOutput{pid, "Non-starter (exec)"}
	} else {
		bot_output_chan <- BotOutput{pid, scanner.Text()}
	}

	for {

		to_send := <- io			// Since this blocks, main() must never send via io unless it knows our last Scan() worked.

		if bot_is_kill == false {

			fmt.Fprintf(i_pipe, to_send)
			if to_send[len(to_send) - 1] != '\n' {
				fmt.Fprintf(i_pipe, "\n")
			}

			if scanner.Scan() == false {
				fmt.Fprintf(os.Stderr, "Bot %d output reached EOF\n", pid)
				bot_is_kill = true
			}

			bot_output_chan <- BotOutput{pid, scanner.Text()}

		} else {

			bot_output_chan <- BotOutput{pid, ""}

		}
	}
}

// -----------------------------------------------------------------------------------------

func main() {

	start_time := time.Now()

	width, height, seed, no_timeout, no_replay, botlist, infile := parse_args()

	var provided_frame *sim.Frame

	if infile != "" {
		provided_frame, seed = sim.FrameFromFile(infile)
		width = provided_frame.Width()
		height = provided_frame.Height()
	}

	turns := turns_from_size(width, height)

	players := len(botlist)

	if provided_frame != nil && provided_frame.Players() != players {
		fmt.Fprintf(os.Stderr, "Wrong number of bots (%d) given for this replay (need %d)\n", players, provided_frame.Players())
		return
	}

	if players < 1 || players > 4 {
		fmt.Fprintf(os.Stderr, "Bad number of players: %d\n", players)
		return
	}

	io_chans := make([]chan string, players)

	for pid := 0; pid < players; pid++ {
		io_chans[pid] = make(chan string)
	}

	var crash_list []int

	for pid := 0; pid < players; pid++ {
		crash_list = append(crash_list, -1)
	}

	constants := sim.NewConstants(players, width, height, turns, seed)
	game := sim.NewGame(constants)

	if provided_frame == nil {
		game.UseFrame(sim.MapGenOfficial(players, width, height, constants.INITIAL_ENERGY, seed))
	} else {
		game.UseFrame(provided_frame)
	}

	json_blob_bytes, _ := json.Marshal(constants)
	json_blob := string(json_blob_bytes)
	json_blob = strings.Replace(json_blob, " ", "", -1)

	init_string := game.BotInitString()

	for pid := 0; pid < players; pid++ {
		pregame := fmt.Sprintf("%s\n%d %d\n%s", json_blob, players, pid, init_string)
		go bot_handler(botlist[pid], pid, io_chans[pid], pregame)
	}

	var player_names []string
	for pid := 0; pid < players; pid++ {
		player_names = append(player_names, "")
	}

	// Get names...

	names_received := 0
	deadline := time.NewTimer(30 * time.Second)

	GetNames:
	for {

		select {

		case op := <- bot_output_chan:

			names_received++
			player_names[op.Pid] = op.Output

			if player_names[op.Pid] == "" {
				player_names[op.Pid] = "(blank)"
			}

			if names_received >= players {
				deadline.Stop()
				break GetNames
			}

		case <- deadline.C:

			if no_timeout {
				continue GetNames
			}

			fmt.Fprintf(os.Stderr, "Hit the deadline. Received: %d\n", names_received)

			for pid := 0; pid < players; pid++ {
				if player_names[pid] == "" {
					player_names[pid] = "Non-starter (time)"
					crash_list[pid] = 0
				}
			}

			break GetNames
		}
	}

	replay := sim.NewReplay(player_names, game, turns, seed)

	move_strings := make([]string, players)

	for turn := 0; turn <= turns; turn++ {				// Don't mess with this now, we expect <= below...

		update_string, rf := game.UpdateFromMoves(move_strings)
		replay.FullFrames = append(replay.FullFrames, rf)

		// Send on every turn except final...

		if turn < turns {
			for pid := 0; pid < players; pid++ {
				if crash_list[pid] == -1 {
					io_chans[pid] <- update_string		// THIS WILL HANG THE ENGINE IF THE HANDLER ISN'T AT THE RIGHT PLACE. Care!
				}
			}
		}

		received := make([]bool, players)
		received_total := 0

		// Count dead players as already received ""
		// Also do this for all bots on the very final frame.

		for pid := 0; pid < players; pid++ {
			if crash_list[pid] != -1 || turn == turns {
				move_strings[pid] = ""
				received[pid] = true
				received_total++
			}
		}

		if received_total < players {

			deadline := time.NewTimer(2 * time.Second)

			Wait:
			for {

				select {

				case op := <- bot_output_chan:

					if crash_list[op.Pid] == -1 {		// Bot hasn't crashed (if it had, we already pretended it sent "")

						received_total++
						received[op.Pid] = true
						move_strings[op.Pid] = op.Output

						if received_total >= players {
							deadline.Stop()
							break Wait
						}
					}

				case <- deadline.C:

					if no_timeout {
						continue Wait
					}

					for pid := 0; pid < players; pid++ {
						if received[pid] == false {
							move_strings[pid] = ""
							crash_list[pid] = turn
						}
					}

					break Wait
				}
			}
		}
	}

	_, rf := game.UpdateFromMoves(move_strings)
	replay.FullFrames = append(replay.FullFrames, rf)

	replay.Stats = new(sim.ReplayStats)
	replay.Stats.NumTurns = turns + 1

	for pid := 0; pid < players; pid++ {

		replay.Stats.Pstats = append(replay.Stats.Pstats, new(sim.PlayerStats))
		replay.Stats.Pstats[pid].Pid = pid
		replay.Stats.Pstats[pid].Rank = game.GetRank(pid)

		replay.Stats.Pstats[pid].FinalProduction = game.Budget(pid)

		turn_last_alive := turns + 1		// Like in official replays
		if crash_list[pid] != -1 {
			turn_last_alive = crash_list[pid]
		}

		replay.Stats.Pstats[pid].LastTurnAlive = turn_last_alive
	}

	all_dropoffs := game.GetDropoffs()

	for _, dropoff := range all_dropoffs {
		replay.Stats.Pstats[dropoff.Owner].HalitePerDropoff = append(replay.Stats.Pstats[dropoff.Owner].HalitePerDropoff, dropoff)

		replay.Stats.Pstats[dropoff.Owner].TotalProduction += dropoff.Gathered

		if dropoff.Factory == false {
			replay.Stats.Pstats[dropoff.Owner].NumDropoffs += 1
		}
	}

	replay_filename := ""

	if no_replay == false {
		if infile != "" {
			replay_filename = fmt.Sprintf("reload-%v-%v-%v.hlt", seed, width, height)
		} else {
			replay_filename = fmt.Sprintf("replay-%v-%v-%v.hlt", seed, width, height)
		}
		replay.Dump(replay_filename)
	}

	type RankScore struct {
		Rank			int					`json:"rank"`
		Score			int					`json:"score"`
	}

	type PrintedStats struct {
		MapWidth		int					`json:"map_width"`
		MapHeight		int					`json:"map_height"`
		Replay			string				`json:"replay"`
		Stats			map[int]RankScore	`json:"stats"`
		MapSeed			int32				`json:"map_seed"`
		Time			string				`json:"time"`
	}

	ps := new(PrintedStats)

	ps.MapWidth = width
	ps.MapHeight = height
	ps.Replay = replay_filename
	ps.MapSeed = seed
	ps.Stats = make(map[int]RankScore)
	ps.Time = time.Now().Sub(start_time).Round(time.Millisecond).String()

	for pid := 0; pid < players; pid++ {

		rankscore := RankScore{
			Rank: replay.Stats.Pstats[pid].Rank,
			Score: replay.Stats.Pstats[pid].FinalProduction,
		}

		ps.Stats[pid] = rankscore
	}

	foo, _ := json.MarshalIndent(ps, "", "    ")

	fmt.Printf(string(foo))
	fmt.Printf("\n")
}

// -----------------------------------------------------------------------------------------

func parse_args() (width, height int, seed int32, no_timeout bool, no_replay bool, botlist []string, infile string) {

	seed = int32(time.Now().UTC().Unix())

	dealt_with := make([]bool, len(os.Args))
	dealt_with[0] = true

	var err error

	for n, arg := range os.Args {

		if dealt_with[n] {
			continue
		}

		if arg == "--width" || arg == "-w" {
			dealt_with[n] = true
			dealt_with[n + 1] = true
			width, err = strconv.Atoi(os.Args[n + 1])
			if err != nil {
				fmt.Fprintf(os.Stderr, "Couldn't understand stated width.\n")
				os.Exit(1)
			}
			continue
		}

		if arg == "--height" || arg == "-h" {
			dealt_with[n] = true
			dealt_with[n + 1] = true
			height, err = strconv.Atoi(os.Args[n + 1])
			if err != nil {
				fmt.Fprintf(os.Stderr, "Couldn't understand stated height.\n")
				os.Exit(1)
			}
			continue
		}

		if arg == "--seed" || arg == "-s" {
			dealt_with[n] = true
			dealt_with[n + 1] = true
			seed64, err := strconv.ParseInt(os.Args[n + 1], 10, 32)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Couldn't understand stated seed.\n")
				os.Exit(1)
			}
			seed = int32(seed64)
			continue
		}

		if arg == "--file" || arg == "-f" {
			dealt_with[n] = true
			dealt_with[n + 1] = true
			infile = os.Args[n + 1]
			continue
		}

		if arg == "--no-timeout" {
			dealt_with[n] = true
			no_timeout = true
			continue
		}

		if arg == "--no-replay" {
			dealt_with[n] = true
			no_replay = true
			continue
		}
	}

	for n, arg := range os.Args {

		if dealt_with[n] {
			continue
		}

		botlist = append(botlist, arg)
	}

	rand.Seed(int64(seed))		// Use the seed to get width/height, if needed...

	if width == 0 && height > 0 { width = height }
	if height == 0 && width > 0 { height = width }

	if width < 16 || width > 64 || height < 16 || height > 64 {
		width = 32 + rand.Intn(5) * 8
		height = width
	}

	return width, height, seed, no_timeout, no_replay, botlist, infile
}

// -----------------------------------------------------------------------------------------

func turns_from_size(width, height int) int {

	size := width
	if height > size {
		size = height
	}

	return (((size - 32) * 25) / 8) + 400
}
