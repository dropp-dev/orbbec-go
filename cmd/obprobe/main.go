// Command obprobe inspects a Gemini 435Le camera.
//
//	obprobe discover                          find cameras on all interfaces
//	obprobe info  -ip 192.168.1.10            device info, profiles, precision and intrinsics (read-only)
//	obprobe grab  -ip 192.168.1.10 -out dir   stream for a few seconds, save depth.png (16-bit, mm) and color.jpg
//	obprobe watch -ip 192.168.1.10            report frame rates every 10 s and reconnect when offline
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"time"

	"github.com/dropp-dev/orbbec-go"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	ip := fs.String("ip", "192.168.1.10", "camera IP")
	out := fs.String("out", ".", "grab: output directory")
	secs := fs.Int("seconds", 5, "grab: streaming duration in seconds")
	fps := fs.Int("fps", 10, "depth frame rate")
	noColor := fs.Bool("no-color", false, "do not start the color stream")
	verbose := fs.Bool("v", false, "debug logging")
	duration := fs.Duration("duration", 0, "watch: stop after this duration (0 = until interrupted)")
	precision := fs.String("precision", "keep", "depth precision: keep, default, or level 0-6")
	_ = fs.Parse(os.Args[2:])

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	var err error
	switch cmd {
	case "discover":
		err = discover(ctx)
	case "info":
		err = info(ctx, *ip, log)
	case "grab":
		err = grab(ctx, newCamera(*ip, *fps, *noColor, *precision, log), *secs, *out)
	case "watch":
		err = watch(ctx, newCamera(*ip, *fps, *noColor, *precision, log), log)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: obprobe discover | info -ip IP | grab -ip IP [-seconds 5] [-out dir] | watch -ip IP [-duration 1h]")
	os.Exit(2)
}

func newCamera(ip string, fps int, noColor bool, precision string, log *slog.Logger) *orbbec.Camera {
	var p *orbbec.DepthPrecision
	switch precision {
	case "default":
	case "keep":
		k := orbbec.PrecisionKeep
		p = &k
	default:
		n, err := strconv.Atoi(precision)
		if err != nil || n < 0 || n > 6 {
			fmt.Fprintln(os.Stderr, "error: -precision must be keep, default or 0-6")
			os.Exit(2)
		}
		k := orbbec.DepthPrecision(n)
		p = &k
	}
	return orbbec.New(orbbec.Config{IP: ip, Logger: log, Depth: orbbec.StreamConfig{Width: 1280, Height: 800, FPS: fps},
		NoColor: noColor, DepthPrecision: p})
}

func printJSON(label string, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Printf("%s: %s\n", label, b)
}

func discover(ctx context.Context) error {
	devs, err := orbbec.Discover(ctx, time.Second)
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		fmt.Println("no camera found (broadcast may be blocked; try info -ip)")
	}
	for _, d := range devs {
		fmt.Printf("%s  %s  SN %s  firmware %s  MAC %s  mask %s  gateway %s  (via %s)\n",
			d.IP, d.Model, d.Serial, d.Firmware, d.MAC, d.Mask, d.Gateway, d.LocalIP)
	}
	return nil
}

func info(ctx context.Context, ip string, log *slog.Logger) error {
	dev, err := orbbec.OpenDevice(ctx, orbbec.DeviceOptions{IP: ip, Logger: log, ReadOnly: true})
	if err != nil {
		return err
	}
	defer dev.Close()
	printJSON("device", dev.Info())
	printJSON("version", dev.Version())
	fmt.Println("profiles:")
	for _, p := range dev.Profiles() {
		fmt.Println("  ", p)
	}
	r, err := dev.GetPropertyRange(orbbec.PropDepthPrecisionLevelInt)
	if err != nil {
		return err
	}
	fmt.Printf("depth precision level: current %d, default %d, range %d-%d; unit %.2f mm\n", r.Cur, r.Def, r.Min, r.Max, dev.DepthUnitMM())
	cal := dev.Calibration()
	fmt.Printf("baseline: %.3f mm\n", cal.BaselineMM())
	for _, sz := range [][2]int{{1280, 800}, {640, 400}} {
		if di, err := cal.DepthIntrinsics(sz[0], sz[1]); err == nil {
			fmt.Printf("depth %dx%d: %+v\n", sz[0], sz[1], di)
		}
		if ci, err := cal.ColorIntrinsics(sz[0], sz[1]); err == nil {
			fmt.Printf("color %dx%d: %+v\n", sz[0], sz[1], ci)
		}
	}
	if ext, ok := cal.DepthToColor(); ok {
		printJSON("depth to color", ext)
	}
	return nil
}

func grab(ctx context.Context, cam *orbbec.Camera, secs int, out string) error {
	t0 := time.Now()
	if err := cam.OpenContext(ctx); err != nil {
		return err
	}
	defer cam.Close()
	fmt.Printf("opened in %v\n", time.Since(t0).Round(time.Millisecond))
	printJSON("device", cam.DeviceInfo())
	printJSON("depth intrinsics", cam.Intrinsics())
	if cp := cam.ColorParams(); cp != nil {
		printJSON("color", cp)
	}

	first, err := cam.GrabDepth(3 * time.Second)
	if err != nil {
		return err
	}
	last := first
	for deadline := time.Now().Add(time.Duration(secs) * time.Second); time.Now().Before(deadline); {
		if last, err = cam.GrabDepth(3 * time.Second); err != nil {
			return err
		}
	}
	n, dt := last.Seq-first.Seq, last.Time.Sub(first.Time)
	fmt.Printf("depth: %d frames in %v (%.1f fps)\n", n, dt.Round(time.Millisecond), float64(n)/dt.Seconds())
	valid := 0
	for _, v := range last.Data {
		if v > 0 {
			valid++
		}
	}
	fmt.Printf("center depth %d mm, valid pixels %.1f%%\n", last.At(last.Width/2, last.Height/2), 100*float64(valid)/float64(len(last.Data)))

	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	img := image.NewGray16(image.Rect(0, 0, last.Width, last.Height))
	for i, v := range last.Data {
		img.Pix[2*i], img.Pix[2*i+1] = byte(v>>8), byte(v)
	}
	f, err := os.Create(filepath.Join(out, "depth.png"))
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Println("saved", f.Name())
	if c, _ := cam.GrabColor(2 * time.Second); c != nil {
		p := filepath.Join(out, "color.jpg")
		if err := os.WriteFile(p, c.JPEG, 0o644); err != nil {
			return err
		}
		fmt.Println("saved", p)
	}
	return nil
}

func watch(ctx context.Context, cam *orbbec.Camera, log *slog.Logger) error {
	if err := cam.OpenContext(ctx); err != nil {
		return err
	}
	defer cam.Close()
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	var depthSeq, colorSeq uint64
	since := time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		if !cam.IsAlive(0) {
			log.Warn("camera offline, reopening", "reason", cam.LastError())
			cam.Close()
			if err := cam.OpenContext(ctx); err != nil {
				log.Error("reopen failed", "err", err)
				continue
			}
			depthSeq, colorSeq, since = 0, 0, time.Now()
			continue
		}
		f, err := cam.GrabDepth(2 * time.Second)
		if err != nil {
			log.Warn("grab failed", "err", err)
			continue
		}
		_, cs := cam.LatestColor(0, 0)
		el := time.Since(since).Seconds()
		log.Info("running", "depth_fps", fmt.Sprintf("%.1f", float64(f.Seq-depthSeq)/el),
			"color_fps", fmt.Sprintf("%.1f", float64(cs-colorSeq)/el), "center_mm", f.At(f.Width/2, f.Height/2))
		depthSeq, colorSeq, since = f.Seq, cs, time.Now()
	}
}
