package cmd

import (
	"bufio"
	"crypto"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/semver"
	"github.com/fatih/color"
	"github.com/keygen-sh/keygen-cli/internal/keygenext"
	"github.com/mattn/go-isatty"
	"github.com/mitchellh/go-homedir"
	"github.com/oasisprotocol/curve25519-voi/primitives/ed25519"
	"github.com/spf13/cobra"
	"github.com/vbauerster/mpb/v7"
	"github.com/vbauerster/mpb/v7/decor"
)

var (
	distOpts = &CommandOptions{}
	distCmd  = &cobra.Command{
		Use:   "dist <path>",
		Short: "publish a new release for a product",
		Example: `  keygen dist build/my-program-1-0-0 \
      --signing-key ~/.keys/keygen.key \
      --account '1fddcec8-8dd3-4d8d-9b16-215cac0f9b52' \
      --product '2313b7e7-1ea6-4a01-901e-2931de6bb1e2' \
      --token 'prod-xxx' \
      --platform 'linux/amd64' \
      --version '1.0.0'

Docs:
  https://keygen.sh/docs/cli/`,
		Args: distArgs,
		RunE: distRun,

		// Encountering an error should not display usage
		SilenceUsage: true,
	}
)

func init() {
	distCmd.Flags().StringVar(&keygenext.Account, "account", "", "your keygen.sh account identifier [$KEYGEN_ACCOUNT_ID=<id>] (required)")
	distCmd.Flags().StringVar(&keygenext.Product, "product", "", "your keygen.sh product identifier [$KEYGEN_PRODUCT_ID=<id>] (required)")
	distCmd.Flags().StringVar(&keygenext.Token, "token", "", "your keygen.sh product token [$KEYGEN_PRODUCT_TOKEN] (required)")
	distCmd.Flags().StringVar(&distOpts.filename, "filename", "", "filename for the release (default grabs basename from <path>)")
	distCmd.Flags().StringVar(&distOpts.filetype, "filetype", "auto", "filetype for the release (default grabs extname from <path>)")
	distCmd.Flags().StringVar(&distOpts.version, "version", "", "version for the release (required)")
	distCmd.Flags().StringVar(&distOpts.name, "name", "", "human-readable name for the release")
	distCmd.Flags().StringVar(&distOpts.description, "description", "", "description for the release (e.g. release notes)")
	distCmd.Flags().StringVar(&distOpts.platform, "platform", "", "platform for the release")
	distCmd.Flags().StringVar(&distOpts.channel, "channel", "stable", "channel for the release, one of: stable, rc, beta, alpha, dev")
	distCmd.Flags().StringVar(&distOpts.signature, "signature", "", "pre-calculated signature for the release (defaults using ed25519ph)")
	distCmd.Flags().StringVar(&distOpts.checksum, "checksum", "", "pre-calculated checksum for the release (defaults using sha-512)")
	distCmd.Flags().StringVar(&distOpts.signingAlgorithm, "signing-algorithm", "ed25519ph", "the signing algorithm to use, one of: ed25519ph, ed25519")
	distCmd.Flags().StringVar(&distOpts.signingKeyPath, "signing-key", "", "path to ed25519 private key for signing the release [$KEYGEN_SIGNING_KEY_PATH=<path>, $KEYGEN_SIGNING_KEY=<key>]")
	distCmd.Flags().BoolVar(&distOpts.noAutoUpgrade, "no-auto-upgrade", false, "disable automatic upgrade checks [$KEYGEN_NO_AUTO_UPGRADE=1]")

	// TODO(ezekg) Accept entitlement codes and entitlement IDs?
	distCmd.Flags().StringSliceVar(&distOpts.entitlements, "entitlements", []string{}, "comma seperated list of entitlement constraints (e.g. --entitlements <id>,<id>,...)")

	// TODO(ezekg) Prompt multi-line description input from stdin if "--"?
	// TODO(ezekg) Add metadata flag

	if v := os.Getenv("KEYGEN_ACCOUNT_ID"); v != "" {
		if keygenext.Account == "" {
			keygenext.Account = v
		}
	}

	if v := os.Getenv("KEYGEN_PRODUCT_ID"); v != "" {
		if keygenext.Product == "" {
			keygenext.Product = v
		}
	}

	if v := os.Getenv("KEYGEN_PRODUCT_TOKEN"); v != "" {
		if keygenext.Token == "" {
			keygenext.Token = v
		}
	}

	if v := os.Getenv("KEYGEN_SIGNING_KEY_PATH"); v != "" {
		if distOpts.signingKeyPath == "" {
			distOpts.signingKeyPath = v
		}
	}

	if v := os.Getenv("KEYGEN_SIGNING_KEY"); v != "" {
		if distOpts.signingKey == "" {
			distOpts.signingKey = v
		}
	}

	if v := os.Getenv("KEYGEN_NO_AUTO_UPGRADE"); v != "" {
		if !distOpts.noAutoUpgrade {
			distOpts.noAutoUpgrade = v == "1" || v == "true"
		}
	}

	if keygenext.Account == "" {
		distCmd.MarkFlagRequired("account")
	}

	if keygenext.Product == "" {
		distCmd.MarkFlagRequired("product")
	}

	if keygenext.Token == "" {
		distCmd.MarkFlagRequired("token")
	}

	distCmd.MarkFlagRequired("version")

	rootCmd.AddCommand(distCmd)
}

func distArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return errors.New("path to file is required")
	}

	return nil
}

func distRun(cmd *cobra.Command, args []string) error {
	if !distOpts.noAutoUpgrade {
		err := upgradeRun(nil, nil)
		if err != nil {
			return err
		}
	}

	path, err := homedir.Expand(args[0])
	if err != nil {
		return fmt.Errorf(`path "%s" is not expandable (%s)`, args[0], err)
	}

	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf(`path "%s" is not readable (%s)`, path, err.(*os.PathError).Err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf(`path "%s" is not readable (%s)`, path, err.(*os.PathError).Err)
	}

	if info.IsDir() {
		return fmt.Errorf(`path "%s" is a directory (must be a file)`, path)
	}

	filename := filepath.Base(info.Name())
	filesize := info.Size()

	// Allow filename to be overridden
	if n := distOpts.filename; n != "" {
		filename = n
	}

	// Allow filetype to be overridden
	var filetype string

	if distOpts.filetype == "auto" {
		filetype = filepath.Ext(filename)
		if _, e := strconv.Atoi(filetype); e == nil || filetype == "" {
			filetype = "bin"
		}
	} else {
		filetype = distOpts.filetype
	}

	channel := distOpts.channel
	platform := distOpts.platform

	constraints := keygenext.Constraints{}
	if e := distOpts.entitlements; len(e) != 0 {
		constraints = constraints.From(e)
	}

	var name *string
	if n := distOpts.name; n != "" {
		name = &n
	}

	var desc *string
	if d := distOpts.description; d != "" {
		desc = &d
	}

	version, err := semver.NewVersion(distOpts.version)
	if err != nil {
		return fmt.Errorf(`version "%s" is not acceptable (%s)`, distOpts.version, strings.ToLower(err.Error()))
	}

	checksum := distOpts.checksum
	if checksum == "" {
		checksum, err = calculateChecksum(file)
		if err != nil {
			return err
		}
	}

	signature := distOpts.signature
	if signature == "" && (distOpts.signingKeyPath != "" || distOpts.signingKey != "") {
		var key string

		switch {
		case distOpts.signingKeyPath != "":
			path, err := homedir.Expand(distOpts.signingKeyPath)
			if err != nil {
				return fmt.Errorf(`signing-key path is not expandable (%s)`, err)
			}

			b, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf(`signing-key path is not readable (%s)`, err)
			}

			key = string(b)
		case distOpts.signingKey != "":
			key = distOpts.signingKey
		}

		signature, err = calculateSignature(key, file)
		if err != nil {
			return err
		}
	}

	release := &keygenext.Release{
		Name:        name,
		Description: desc,
		Version:     version.String(),
		Filename:    filename,
		Filesize:    filesize,
		Filetype:    filetype,
		Platform:    platform,
		Signature:   signature,
		Checksum:    checksum,
		Channel:     channel,
		ProductID:   keygenext.Product,
		Constraints: constraints,
	}

	// TODO(ezekg) Should we do a Create() unless a --upsert flag is given?
	if err := release.Upsert(); err != nil {
		e, ok := err.(*keygenext.APIError)
		if ok {
			italic := color.New(color.Italic).SprintFunc()
			code := e.Code
			if code == "" {
				code = "API_ERROR"
			}

			return fmt.Errorf("%s - %s: %s", italic(code), e.Title, e.Detail)
		}

		return err
	}

	// Create a buffered reader to limit memory footprint
	var reader io.Reader = bufio.NewReaderSize(file, 1024*1024*50 /* 50 mb */)
	var progress *mpb.Progress

	// Create a progress bar for file upload if TTY
	if isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()) {
		progress = mpb.New(mpb.WithWidth(60), mpb.WithRefreshRate(180*time.Millisecond))
		bar := progress.Add(
			release.Filesize,
			mpb.NewBarFiller(mpb.BarStyle().Rbound("|")),
			mpb.BarRemoveOnComplete(),
			mpb.PrependDecorators(
				decor.CountersKibiByte("% .2f / % .2f"),
			),
			mpb.AppendDecorators(
				decor.EwmaETA(decor.ET_STYLE_GO, 90),
				decor.Name(" ] "),
				decor.EwmaSpeed(decor.UnitKiB, "% .2f", 60),
			),
		)

		// Create proxy reader for the progress bar
		reader = bar.ProxyReader(reader)
		closer, ok := reader.(io.ReadCloser)
		if ok {
			defer closer.Close()
		}
	}

	if err := release.Upload(reader); err != nil {
		return err
	}

	if progress != nil {
		progress.Wait()
	}

	italic := color.New(color.Italic).SprintFunc()

	fmt.Println("published release " + italic(release.ID))

	return nil
}

func calculateChecksum(file *os.File) (string, error) {
	defer file.Seek(0, io.SeekStart) // reset reader

	h := sha512.New()

	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}

	digest := h.Sum(nil)

	return base64.RawStdEncoding.EncodeToString(digest), nil
}

func calculateSignature(encSigningKey string, file *os.File) (string, error) {
	defer file.Seek(0, io.SeekStart) // reset reader

	decSigningKey, err := hex.DecodeString(encSigningKey)
	if err != nil {
		return "", fmt.Errorf("bad signing key (%s)", err)
	}

	if l := len(decSigningKey); l != ed25519.PrivateKeySize {
		return "", fmt.Errorf("bad signing key length (got %d expected %d)", l, ed25519.PrivateKeySize)
	}

	signingKey := ed25519.PrivateKey(decSigningKey)
	var sig []byte

	switch distOpts.signingAlgorithm {
	case "ed25519ph":
		// We're using Ed25519ph which expects a pre-hashed message using SHA-512
		h := sha512.New()

		if _, err := io.Copy(h, file); err != nil {
			return "", err
		}

		opts := &ed25519.Options{Hash: crypto.SHA512, Context: keygenext.Product}
		digest := h.Sum(nil)

		sig, err = signingKey.Sign(nil, digest, opts)
		if err != nil {
			return "", err
		}
	case "ed25519":
		fmt.Println("warning: using ed25519 to sign large files is not recommended (use ed25519ph instead)")

		b, err := ioutil.ReadAll(file)
		if err != nil {
			return "", err
		}

		sig, err = signingKey.Sign(nil, b, &ed25519.Options{})
		if err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf(`signing algorithm "%s" is not supported`, distOpts.signingAlgorithm)
	}

	return base64.RawStdEncoding.EncodeToString(sig), nil
}
