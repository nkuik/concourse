package tsacmd

import (
	"bytes"
	"fmt"
	"net/http"
	"sync"
	"time"

	"code.cloudfoundry.org/lager"
	"github.com/concourse/concourse"
	"github.com/concourse/concourse/atc"
	"github.com/concourse/concourse/tsa"
	"github.com/concourse/flag"
	"github.com/tedsuo/ifrit"
	"golang.org/x/crypto/ssh"
)

type TSACommand struct {
	Logger flag.Lager

	BindIP      flag.IP `long:"bind-ip"   default:"0.0.0.0" description:"IP address on which to listen for SSH."`
	PeerAddress string  `long:"peer-address" default:"127.0.0.1" description:"Network address of this web node, reachable by other web nodes. Used for forwarded worker addresses."`
	BindPort    uint16  `long:"bind-port" default:"2222"    description:"Port on which to listen for SSH."`

	DebugBindIP   flag.IP `long:"debug-bind-ip"   default:"127.0.0.1" description:"IP address on which to listen for the pprof debugger endpoints."`
	DebugBindPort uint16  `long:"debug-bind-port" default:"2221"      description:"Port on which to listen for the pprof debugger endpoints."`

	HostKey            *flag.PrivateKey               `long:"host-key"        required:"true" description:"Path to private key to use for the SSH server."`
	AuthorizedKeys     flag.AuthorizedKeys            `long:"authorized-keys" description:"Path to file containing keys to authorize, in SSH authorized_keys format (one public key per line)."`
	TeamAuthorizedKeys map[string]flag.AuthorizedKeys `long:"team-authorized-keys" value-name:"NAME:PATH" description:"Path to file containing keys to authorize, in SSH authorized_keys format (one public key per line)."`

	ATCURLs []flag.URL `long:"atc-url" required:"true" description:"ATC API endpoints to which workers will be registered."`

	SessionSigningKey *flag.PrivateKey `long:"session-signing-key" required:"true" description:"Path to private key to use when signing tokens in reqests to the ATC during registration."`

	HeartbeatInterval time.Duration `long:"heartbeat-interval" default:"30s" description:"interval on which to heartbeat workers to the ATC"`
}

type TeamAuthKeys struct {
	Team     string
	AuthKeys []ssh.PublicKey
}

func (cmd *TSACommand) Runner(args []string, webConfig concourse.WebConfig) (ifrit.Runner, error) {
	logger, _ := cmd.constructLogger(webConfig)

	atcEndpointPicker := tsa.NewRandomATCEndpointPicker(cmd.ATCURLs)

	teamAuthorizedKeys, err := cmd.loadTeamAuthorizedKeys()
	if err != nil {
		return nil, fmt.Errorf("failed to load team authorized keys: %s", err)
	}

	if len(cmd.AuthorizedKeys.Keys)+len(cmd.TeamAuthorizedKeys) == 0 {
		logger.Info("starting-tsa-without-authorized-keys")
	}

	sessionAuthTeam := &sessionTeam{
		sessionTeams: make(map[string]string),
		lock:         &sync.RWMutex{},
	}

	config, err := cmd.configureSSHServer(sessionAuthTeam, cmd.AuthorizedKeys.Keys, teamAuthorizedKeys)
	if err != nil {
		return nil, fmt.Errorf("failed to configure SSH server: %s", err)
	}

	listenAddr := fmt.Sprintf("%s:%d", cmd.BindIP, cmd.BindPort)

	if cmd.SessionSigningKey == nil {
		return nil, fmt.Errorf("missing session signing key")
	}

	tokenGenerator := tsa.NewTokenGenerator(cmd.SessionSigningKey.PrivateKey)

	server := &server{
		logger:            logger,
		heartbeatInterval: cmd.HeartbeatInterval,
		cprInterval:       1 * time.Second,
		atcEndpointPicker: atcEndpointPicker,
		tokenGenerator:    tokenGenerator,
		forwardHost:       cmd.PeerAddress,
		config:            config,
		httpClient:        http.DefaultClient,
		sessionTeam:       sessionAuthTeam,
	}

	return serverRunner{logger, server, listenAddr}, nil
}

func (cmd *TSACommand) constructLogger(config concourse.WebConfig) (lager.Logger, *lager.ReconfigurableSink) {
	logger, reconfigurableSink := cmd.Logger.Logger("tsa")
	if config.LogClusterName {
		logger = logger.WithData(lager.Data{
			"cluster": config.ClusterName,
		})
	}

	return logger, reconfigurableSink
}

func (cmd *TSACommand) loadTeamAuthorizedKeys() ([]TeamAuthKeys, error) {
	var teamKeys []TeamAuthKeys

	for teamName, keys := range cmd.TeamAuthorizedKeys {
		teamKeys = append(teamKeys, TeamAuthKeys{
			Team:     teamName,
			AuthKeys: keys.Keys,
		})
	}

	return teamKeys, nil
}

func (cmd *TSACommand) configureSSHServer(sessionAuthTeam *sessionTeam, authorizedKeys []ssh.PublicKey, teamAuthorizedKeys []TeamAuthKeys) (*ssh.ServerConfig, error) {
	certChecker := &ssh.CertChecker{
		IsUserAuthority: func(key ssh.PublicKey) bool {
			return false
		},

		IsHostAuthority: func(key ssh.PublicKey, address string) bool {
			return false
		},

		UserKeyFallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			for _, k := range authorizedKeys {
				if bytes.Equal(k.Marshal(), key.Marshal()) {
					return nil, nil
				}
			}

			for _, teamKeys := range teamAuthorizedKeys {
				for _, k := range teamKeys.AuthKeys {
					if bytes.Equal(k.Marshal(), key.Marshal()) {
						sessionAuthTeam.AuthorizeTeam(string(conn.SessionID()), teamKeys.Team)
						return nil, nil
					}
				}
			}

			return nil, fmt.Errorf("unknown public key")
		},
	}

	config := &ssh.ServerConfig{
		Config: atc.DefaultSSHConfig(),
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			return certChecker.Authenticate(conn, key)
		},
	}

	signer, err := ssh.NewSignerFromKey(cmd.HostKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create signer from host key: %s", err)
	}

	config.AddHostKey(signer)

	return config, nil
}

func (cmd *TSACommand) debugBindAddr() string {
	return fmt.Sprintf("%s:%d", cmd.DebugBindIP, cmd.DebugBindPort)
}
