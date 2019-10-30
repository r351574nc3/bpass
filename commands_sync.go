package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aarondl/bpass/crypt"
	"github.com/aarondl/bpass/scpsync"
	"github.com/aarondl/bpass/txblob"
	"github.com/aarondl/bpass/txformat"

	"golang.org/x/crypto/ssh"
)

const (
	syncSCP = "scp"
)

var (
	errNotFound = errors.New("not found")
)

func (u *uiContext) sync(auto, push bool) error {
	err := u.store.UpdateSnapshot()
	if err != nil {
		return err
	}

	// From here on we need to avoid updating the store snapshot until we're
	// done syncing all the accounts, otherwise we run the risk of downloading
	// and running a new sync partway through and that's unexpected behavior
	syncs, err := u.collectSyncs()
	if err != nil {
		return err
	}

	// From this point on we don't worry about key's not being present for
	// the most part since collectSyncs should only return valid things
	hosts := make(map[string]string)
	logs := make([][]txformat.Tx, 0, len(syncs))
	for _, uuid := range syncs {
		entry := u.store.Snapshot[uuid]
		name, _ := entry[txblob.KeyName]

		infoColor.Println("pull:", name)

		ct, hostentry, err := pullBlob(u, uuid)

		// Add to known hosts
		if len(hostentry) != 0 {
			hosts[uuid] = hostentry
		}

		if err != nil {
			if err != errNotFound {
				errColor.Printf("error pulling %q: %v\n", name, err)
			}
			continue
		}

		pt, err := decryptBlob(u, name, ct)
		if err != nil {
			errColor.Println("failed to decode %q: %v\n", name, err)
			continue
		}

		log, err := txformat.NewLog(pt)
		if err != nil {
			errColor.Println("failed parsing log %q: %v\n", name, err)
			continue
		}

		logs = append(logs, log)
	}

	out, err := mergeLogs(u, u.store.Log, logs)
	if err != nil {
		errColor.Println("aborting sync, failed to merge logs:", err)
		return nil
	}

	u.store.ResetSnapshot()
	u.store.Log = out
	if err = u.store.UpdateSnapshot(); err != nil {
		errColor.Println("failed to rebuild snapshot, poisoned by sync:", err)
		errColor.Println("exiting to avoid corrupting local file")
		os.Exit(1)
	}

	if err = saveHosts(u.store.Store, hosts); err != nil {
		return err
	}

	if !push {
		return nil
	}

	// Save & encrypt in memory
	var pt, ct []byte
	if pt, err = u.store.Save(); err != nil {
		return err
	}
	if ct, err = crypt.Encrypt(cryptVersion, u.key, u.salt, pt); err != nil {
		return err
	}

	// Push back to other machines
	hosts = make(map[string]string)
	for _, uuid := range syncs {
		entry := u.store.Snapshot[uuid]
		name, _ := entry[txblob.KeyName]

		infoColor.Println("push:", name)

		hostentry, err := pushBlob(u, uuid, ct)
		if err != nil {
			errColor.Printf("error pushing to %q: %v\n", name, err)
		}

		if len(hostentry) != 0 {
			hosts[uuid] = hostentry
		}
	}

	if err = saveHosts(u.store.Store, hosts); err != nil {
		return err
	}

	return nil
}

func saveHosts(store *txformat.Store, newHosts map[string]string) error {
	for uuid, hostentry := range newHosts {
		entry := store.Snapshot[uuid]
		hosts := entry[txblob.KeyKnownHosts]
		if len(hosts) == 0 {
			store.Set(uuid, txblob.KeyKnownHosts, hostentry)
			continue
		}

		hostLines := strings.Split(hosts, "\n")
		hostLines = append(hostLines, hostentry)
		store.Set(uuid, txblob.KeyKnownHosts, strings.Join(hostLines, "\n"))
	}

	return store.UpdateSnapshot()
}

// collectSyncs attempts to gather automatic sync entries and ensure that basic
// attributes are available (name, path, synckind) to make it easier to use
// later
func (u *uiContext) collectSyncs() ([]string, error) {
	var validSyncs []string

	for uuid, entry := range u.store.Snapshot {
		sync, _ := entry[txblob.KeySync]
		if sync != "true" {
			continue
		}

		name := entry[txblob.KeyName]
		if len(name) == 0 {
			errColor.Printf("%q is a sync entry but its name is broken (skipping)", uuid)
			continue
		}

		uri := entry[txblob.KeyURL]
		if len(uri) == 0 {
			errColor.Printf("%q is a sync entry but it has no %q key (skipping)\n", name, txblob.KeyURL)
			continue
		}

		u, err := url.Parse(uri)
		if err != nil {
			errColor.Printf("%q is a sync account but it's url is not parseable (skipping)\n", name)
			continue
		}

		switch u.Scheme {
		case syncSCP:
			validSyncs = append(validSyncs, uuid)
		default:
			errColor.Printf("entry %q is a %q sync account, but this kind is unknown (old bpass version?)\n", name, u.Scheme)
		}
	}

	return validSyncs, nil
}

// pullBlob tries to download a file from the given sync entry
func pullBlob(u *uiContext, uuid string) (ct []byte, hostentry string, err error) {
	entry := u.store.Snapshot[uuid]
	// We know this parses because we parsed it once before
	uri, _ := url.Parse(entry[txblob.KeyURL])

	switch uri.Scheme {
	case syncSCP:
		hostentry, ct, err = u.sshPull(entry)
		if scpsync.IsNotFoundErr(err) {
			return nil, hostentry, errNotFound
		}
	}

	if err != nil {
		return nil, hostentry, err
	}

	return ct, hostentry, nil
}

// pushBlob uploads a file to a given sync entry
func pushBlob(u *uiContext, uuid string, payload []byte) (hostentry string, err error) {
	entry := u.store.Snapshot[uuid]
	uri, _ := url.Parse(entry[txblob.KeyURL])

	switch uri.Scheme {
	case syncSCP:
		hostentry, err = u.sshPush(entry, payload)
	}

	return hostentry, err
}

func decryptBlob(u *uiContext, name string, ct []byte) (pt []byte, err error) {
	pass := u.pass
	for {
		// Decrypt payload with our loaded key
		_, pt, err = crypt.Decrypt([]byte(pass), ct)
		if err == nil {
			return pt, err
		}

		if err != crypt.ErrWrongPassphrase {
			return nil, err
		}

		pass, err = u.prompt(inputPromptColor.Sprintf("%s passphrase: ", name))

		if err != nil || len(pass) == 0 {
			return nil, nil
		}
	}
}

func mergeLogs(u *uiContext, in []txformat.Tx, toMerge [][]txformat.Tx) ([]txformat.Tx, error) {
	if len(toMerge) == 0 {
		return in, nil
	}

	var c []txformat.Tx
	var conflicts []txformat.Conflict
	for _, log := range toMerge {
		c, conflicts = txformat.Merge(in, log, conflicts)

		if len(conflicts) == 0 {
			break
		}

		infoColor.Println(len(conflicts), " conflicts occurred during syncing!")

		for i, c := range conflicts {
			infoColor.Printf("entry %q was deleted at: %s\nbut at %s, ",
				c.DeleteTx.UUID,
				time.Unix(0, c.DeleteTx.Time).Format(time.RFC3339),
				time.Unix(0, c.SetTx.Time).Format(time.RFC3339),
			)

			switch c.SetTx.Kind {
			case txformat.TxSetKey:
				infoColor.Printf("a kv set happened:\n%s = %s\n",
					c.SetTx.Key,
					c.SetTx.Value,
				)
			case txformat.TxDeleteKey:
				infoColor.Printf("a key delete happened for key:\n%s\n",
					c.SetTx.Key,
				)
			}

			for {
				line, err := u.prompt("[R]estore item? [D]elete item? (r/R/d/D): ")
				if err != nil {
					return nil, err
				}

				switch line {
				case "R", "r":
					conflicts[i].Restore()
				case "D", "d":
					conflicts[i].Delete()
				default:
					continue
				}
			}
		}
	}

	return c, nil
}

func (u *uiContext) sshPull(entry txformat.Entry) (hostentry string, ct []byte, err error) {
	address, path, config, err := sshConfig(entry)
	if err != nil {
		return "", nil, err
	}

	known := entry[txblob.KeyKnownHosts]
	asker := &hostAsker{u: u, known: known}
	config.HostKeyCallback = asker.callback

	payload, err := scpsync.Recv(address, config, path)
	if err != nil {
		return asker.newHost, nil, err
	}

	return asker.newHost, payload, nil
}

func (u *uiContext) sshPush(entry txformat.Entry, ct []byte) (hostentry string, err error) {
	address, path, config, err := sshConfig(entry)
	if err != nil {
		return "", err
	}

	known := entry[txblob.KeyKnownHosts]
	asker := &hostAsker{u: u, known: known}
	config.HostKeyCallback = asker.callback

	err = scpsync.Send(address, config, path, 0600, ct)
	if err != nil {
		return "", err
	}

	return asker.newHost, nil
}

func sshConfig(entry txformat.Entry) (address, path string, config *ssh.ClientConfig, err error) {
	uri, err := url.Parse(entry[txblob.KeyURL])
	if err != nil {
		return "", "", nil, err
	}

	host := uri.Hostname()
	port := uri.Port()
	user := uri.User.Username()
	pass, _ := uri.User.Password()
	secretKey := entry[txblob.KeyPriv]
	path = uri.Path[1:]

	if len(user) == 0 {
		return "", "", nil, errors.New("url missing user")
	}
	if len(host) == 0 {
		return "", "", nil, errors.New("url missing host")
	}
	if len(path) == 0 {
		return "", "", nil, errors.New("url missing file path")
	}

	address = net.JoinHostPort(host, port)
	config = new(ssh.ClientConfig)
	config.User = user
	if len(pass) != 0 {
		config.Auth = append(config.Auth, ssh.Password(pass))
	}

	if len(secretKey) != 0 {
		signer, err := ssh.ParsePrivateKey([]byte(secretKey))
		if err != nil {
			return "", "", nil, err
		}
		config.Auth = append(config.Auth, ssh.PublicKeys(signer))
	}

	return address, path, config, nil
}

type hostAsker struct {
	u       *uiContext
	known   string
	newHost string
}

func (h *hostAsker) callback(hostname string, remote net.Addr, key ssh.PublicKey) error {
	// Format is `hostname address key-type key:base64`
	keyHashBytes := sha256.Sum256(key.Marshal())
	keyHash := fmt.Sprintf("%x", keyHashBytes)

	keyType := key.Type()
	addr := remote.String()
	hostLine := fmt.Sprintf(`%s %s %s %s`, hostname, addr, keyType, keyHash)

	knownLines := strings.Split(h.known, "\n")

	for _, h := range knownLines {
		vals := strings.Split(h, " ")

		if vals[0] != hostname {
			continue
		}

		// Same host, double check key is same
		if vals[2] != keyType {
			return errors.New("known host's key type has changed, could be a mitm attack")
		}
		if vals[3] != keyHash {
			return errors.New("known host's key has changed, could be a mitm attack")
		}

		// We've seen this host before and everything is OK
		return nil
	}

	var b strings.Builder
	for i := 0; i < len(keyHash)-1; i += 2 {
		if i != 0 {
			b.WriteByte(':')
		}
		b.WriteByte(keyHash[i])
		b.WriteByte(keyHash[i+1])
	}
	sha256FingerPrint := b.String()

	infoColor.Printf("(ssh) connected to: %s (%s)\nverify pubkey: %s %s\n",
		hostname, addr, keyType, sha256FingerPrint)
	line, err := h.u.prompt(inputPromptColor.Sprint("Save this host (y/N): "))
	if err != nil {
		return fmt.Errorf("failed to get user confirmation on host: %w", err)
	}

	switch line {
	case "y", "Y":
		h.newHost = hostLine
		return nil
	default:
		return errors.New("user rejected host")
	}
}

func (u *uiContext) syncAddInterruptible(kind string) error {
	err := u.syncAdd(kind)
	switch err {
	case nil:
		return nil
	case ErrEnd:
		errColor.Println("Aborted")
		return nil
	default:
		return err
	}
}

func (u *uiContext) syncAdd(kind string) error {
	found := false
	for _, k := range []string{syncSCP} {
		if k == kind {
			found = true
			break
		}
	}

	if !found {
		errColor.Printf("%q is not a supported sync kind (old version of bpass?)\n", kind)
		return nil
	}

	return u.store.Do(func() error {
		// New entry
		uuid, err := u.store.NewSync(kind)
		if err != nil {
			return err
		}

		user, err := u.getString("user")
		if err != nil {
			return err
		}

		host, err := u.getString("host")
		if err != nil {
			return err
		}

		port := "22"
		for {
			port, err = u.prompt(inputPromptColor.Sprint("port (22): "))
			if err != nil {
				return err
			}

			if len(port) == 0 {
				port = "22"
				break
			}

			_, err = strconv.Atoi(port)
			if err != nil {
				errColor.Printf("port must be an integer between %d and %d\n", 1, int(math.MaxUint16)-1)
				continue
			}

			break
		}

		file, err := u.getString("path")
		if err != nil {
			return err
		}

		var uri url.URL
		uri.Scheme = kind
		uri.User = url.User(user)
		uri.Host = net.JoinHostPort(host, port)
		uri.Path = file

		inputPromptColor.Println("Key type:")
		choice, err := u.getMenuChoice(inputPromptColor.Sprint("> "), []string{"ED25519", "RSA 4096", "Password"})
		if err != nil {
			return err
		}

		switch choice {
		case 0:
			pub, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				errColor.Println("failed to generate ed25519 ssh key")
				return nil
			}

			// Marshal private key into DER ASN.1 then to PEM
			b, err := x509.MarshalPKCS8PrivateKey(priv)
			if err != nil {
				errColor.Println("failed to marshal ed25519 private key with x509:", err)
			}
			pemBlock := pem.Block{Type: "PRIVATE KEY", Bytes: b}
			b = pem.EncodeToMemory(&pemBlock)

			public, err := ssh.NewPublicKey(pub)
			if err != nil {
				errColor.Println("failed to parse public key:", err)
			}
			publicStr := string(bytes.TrimSpace(ssh.MarshalAuthorizedKey(public))) + " @bpass"

			u.store.Set(uuid, txblob.KeyPriv, string(bytes.TrimSpace(b)))
			u.store.Set(uuid, txblob.KeyPub, publicStr)

			infoColor.Printf("successfully generated new ed25519 key:\n%s\n", publicStr)

		case 1:
			priv, err := rsa.GenerateKey(rand.Reader, 4096)
			if err != nil {
				errColor.Println("failed to generate rsa-4096 ssh key")
				return nil
			}

			// Marshal private key into DER ASN.1 then to PEM
			b, err := x509.MarshalPKCS8PrivateKey(priv)
			if err != nil {
				errColor.Println("failed to marshal rsa private key with x509:", err)
				return nil
			}
			pemBlock := pem.Block{Type: "PRIVATE KEY", Bytes: b}
			b = pem.EncodeToMemory(&pemBlock)

			public, err := ssh.NewPublicKey(&priv.PublicKey)
			if err != nil {
				errColor.Println("failed to parse public key:", err)
			}
			publicStr := string(bytes.TrimSpace(ssh.MarshalAuthorizedKey(public))) + " @bpass"

			u.store.Set(uuid, txblob.KeyPriv, string(bytes.TrimSpace(b)))
			u.store.Store.Set(uuid, txblob.KeyPub, publicStr)

			infoColor.Printf("successfully generated new rsa-4096 key:\n%s\n", publicStr)

		case 2:
			pass, err := u.getPassword()
			if err != nil {
				return err
			}

			uri.User = url.UserPassword(user, pass)
		default:
			panic("how did this happen?")
		}

		// Use raw-er sets to avoid timestamp spam
		u.store.Store.Set(uuid, txblob.KeySync, "true")
		u.store.Store.Set(uuid, txblob.KeyURL, uri.String())

		blob, err := u.store.Get(uuid)
		if err != nil {
			return err
		}

		fmt.Println()
		infoColor.Println("added new sync entry:", blob.Name())

		return nil
	})
}
