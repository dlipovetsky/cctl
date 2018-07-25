package cmd

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"text/template"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ghodss/yaml"

	"github.com/spf13/cobra"

	"github.com/platform9/cctl/common"
	sshutil "github.com/platform9/cctl/pkg/util/ssh"

	spv1 "github.com/platform9/ssh-provider/pkg/apis/sshprovider/v1alpha1"
	machineActuator "github.com/platform9/ssh-provider/pkg/clusterapi/machine"
	sputil "github.com/platform9/ssh-provider/pkg/controller"
	sshmachine "github.com/platform9/ssh-provider/pkg/machine"
	setsutil "github.com/platform9/ssh-provider/pkg/util/sets"

	clustercommon "sigs.k8s.io/cluster-api/pkg/apis/cluster/common"
	clusterv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
	clusterutil "sigs.k8s.io/cluster-api/pkg/util"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	drainTimeout            time.Duration
	drainGracePeriodSeconds int
)

// machineCmdCreate represents the machine create command
var machineCmdCreate = &cobra.Command{
	Use:   "machine",
	Short: "Adds a machine to the cluster",
	Run: func(cmd *cobra.Command, args []string) {
		ip := cmd.Flag("ip").Value.String()
		iface := cmd.Flag("iface").Value.String()
		role := clustercommon.MachineRole(strings.Title(cmd.Flag("role").Value.String()))
		// TODO(dlipovetsky) Move to master validation code
		if role != clustercommon.MasterRole && role != clustercommon.NodeRole {
			log.Fatalf("Machine role %q is not supported, must be %q or %q.", role, clustercommon.MasterRole, clustercommon.NodeRole)
		}
		port, err := strconv.Atoi(cmd.Flag("port").Value.String())
		if err != nil {
			log.Fatalf("Invalid port %v", err)
		}
		publicKeyFiles := cmd.Flag("publicKeys").Value.String()

		var publicKeys []string
		for _, file := range strings.Split(publicKeyFiles, ",") {
			publicKey, err := sshutil.PublicKeyFromFile(file)
			if err != nil {
				log.Fatalf("Unable to parse SSH public key from %q: %v", file, err)
			}
			publicKeys = append(publicKeys, string(ssh.MarshalAuthorizedKey(publicKey)))
		}

		cluster, err := state.ClusterClient.ClusterV1alpha1().Clusters(common.DefaultNamespace).Get(common.DefaultClusterName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				log.Fatalf("No cluster found. Create a cluster before creating a machine.")
			}
			log.Fatalf("Unable to get cluster: %v", err)
		}
		sshCredentialSecret, err := state.KubeClient.CoreV1().Secrets(common.DefaultNamespace).Get(common.DefaultSSHCredentialSecretName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				log.Fatalf("No SSH credential found. Create a credential before creating a machine.")
			}
			log.Fatalf("Unable to get SSH credential secret: %v", err)
		}

		newSSHConfig := spv1.SSHConfig{
			Host:       ip,
			Port:       port,
			PublicKeys: publicKeys,
			CredentialSecret: corev1.LocalObjectReference{
				Name: sshCredentialSecret.Name,
			},
		}

		newProvisionedMachine, newMachine, err := newProvisionedMachineAndMachine(ip, role, iface, newSSHConfig)
		if _, err := state.SPClient.SshproviderV1alpha1().ProvisionedMachines(common.DefaultNamespace).Create(newProvisionedMachine); err != nil {
			log.Fatalf("Unable to create provisioned machine: %v", err)
		}
		if _, err := state.ClusterClient.ClusterV1alpha1().Machines(common.DefaultNamespace).Create(newMachine); err != nil {
			log.Fatalf("Unable to create machine: %v", err)
		}

		var masterMachine *clusterv1.Machine
		var masterProvisionedMachine *spv1.ProvisionedMachine
		if clusterutil.RoleContains(clustercommon.NodeRole, newMachine.Spec.Roles) {
			var err error
			masterMachine, masterProvisionedMachine, err = masterMachineAndProvisionedMachine()
			if err != nil {
				log.Fatalf("Unable to get a master machine and provisioned machine: %v", err)
			}
		}

		if clusterutil.RoleContains(clustercommon.NodeRole, newMachine.Spec.Roles) {
			log.Println("Getting a bootstrap token from a master")
			newBootstrapTokenSecret, err := bootstrapTokenSecretFromMachine(masterMachine, masterProvisionedMachine)
			if err != nil {
				log.Fatalf("Unable to read bootstrap token from master: %v", err)
			}
			if _, err := state.KubeClient.CoreV1().Secrets(common.DefaultNamespace).Get(common.DefaultBootstrapTokenSecretName, metav1.GetOptions{}); err != nil {
				if !apierrors.IsNotFound(err) {
					log.Fatalf("Unable to get bootstrap token secret: %v", err)
				}
				if _, err := state.KubeClient.CoreV1().Secrets(common.DefaultNamespace).Create(newBootstrapTokenSecret); err != nil {
					log.Fatalf("Unable to create bootstrap token secret: %v", err)
				}
			} else {
				if _, err := state.KubeClient.CoreV1().Secrets(common.DefaultNamespace).Update(newBootstrapTokenSecret); err != nil {
					log.Fatalf("Unable to update bootstrap token secret: %v", err)
				}
			}
		}

		machineClientBuilder := sshmachine.NewClient
		insecureIgnoreHostKey := false
		if len(publicKeys) == 0 {
			insecureIgnoreHostKey = true
			log.Printf("Not able to verify machine SSH identity: No public keys given. Continuing...")
		}
		actuator := machineActuator.NewActuator(
			state.KubeClient,
			state.ClusterClient,
			state.SPClient,
			machineClientBuilder,
			insecureIgnoreHostKey,
		)
		if err = actuator.Create(cluster, newMachine); err != nil {
			log.Fatalf("Unable to create machine: %v", err)
		}

		if clusterutil.RoleContains(clustercommon.NodeRole, newMachine.Spec.Roles) {
			log.Println("Writing admin kubeconfig to machine")
			kubeconfig, err := adminKubeconfigFromMachine(masterMachine, masterProvisionedMachine)
			if err != nil {
				log.Fatalf("Unable to get admin kubeconfig from master: %v", err)
			}
			if err := writeAdminKubeconfigToMachine(kubeconfig, newMachine, newProvisionedMachine); err != nil {
				log.Fatalf("Unable to write admin kubeconfig to machine: %v", err)
			}
		}

		log.Println("Updating cluster status")
		machineStatus, err := sputil.GetMachineStatus(*newMachine)
		if err != nil {
			log.Fatalf("Unable to get machine %q status: %v", newMachine.Name, err)
		}
		if machineStatus.EtcdMember != nil {
			clusterStatus, err := sputil.GetClusterStatus(*cluster)
			if err != nil {
				log.Fatalf("Unable to get cluster status: %v", err)
			}

			etcdMemberSet := setsutil.NewEtcdMemberSet(clusterStatus.EtcdMembers...)
			etcdMemberSet.Insert(*machineStatus.EtcdMember)
			clusterStatus.EtcdMembers = etcdMemberSet.List()

			if err := sputil.PutClusterStatus(*clusterStatus, cluster); err != nil {
				log.Fatalf("Unable to update cluster status: %v", err)
			}
			if _, err := state.ClusterClient.ClusterV1alpha1().Clusters(common.DefaultNamespace).UpdateStatus(cluster); err != nil {
				log.Fatalf("Unable to update cluster: %v", err)
			}
		}

		if err := state.PullFromAPIs(); err != nil {
			log.Fatalf("Unable to sync on-disk state: %v", err)
		}

		log.Println("Machine created successfully.")
	},
}

func newProvisionedMachineAndMachine(name string, role clustercommon.MachineRole, vipNetworkInterface string, sshConfig spv1.SSHConfig) (*spv1.ProvisionedMachine, *clusterv1.Machine, error) {
	newProvisionedMachine := spv1.ProvisionedMachine{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ProvisionedMachine",
			APIVersion: "sshprovider.platform9.com/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         common.DefaultNamespace,
			CreationTimestamp: metav1.Now(),
		},
		Spec: spv1.ProvisionedMachineSpec{
			SSHConfig:           &sshConfig,
			VIPNetworkInterface: vipNetworkInterface,
		},
	}

	newMachine := clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Machine",
			APIVersion: "cluster.k8s.io/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         common.DefaultNamespace,
			CreationTimestamp: metav1.Now(),
		},
		Spec: clusterv1.MachineSpec{
			Roles: []clustercommon.MachineRole{role},
		},
		Status: clusterv1.MachineStatus{},
	}

	machineProviderSpec := spv1.MachineSpec{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "sshprovider.platform9.com/v1alpha1",
			Kind:       "MachineSpec",
		},
		ProvisionedMachineName: newProvisionedMachine.Name,
		Roles: []spv1.MachineRole{
			spv1.MachineRole(role),
		},
	}
	if err := sputil.PutMachineSpec(machineProviderSpec, &newMachine); err != nil {
		return nil, nil, fmt.Errorf("unable to encode machine provider spec: %v", err)
	}

	machineProviderStatus := spv1.MachineStatus{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "sshprovider.platform9.com/v1alpha1",
			Kind:       "MachineStatus",
		},
	}
	if err := sputil.PutMachineStatus(machineProviderStatus, &newMachine); err != nil {
		return nil, nil, fmt.Errorf("unable to encode machine provider status: %v", err)
	}

	if err := sputil.BindMachineAndProvisionedMachine(&newMachine, &newProvisionedMachine); err != nil {
		return nil, nil, fmt.Errorf("unable to create bi-directional bind between machine and provisioned machine: %v", err)
	}
	return &newProvisionedMachine, &newMachine, nil
}

var machineCmdDelete = &cobra.Command{
	Use:   "machine",
	Short: "Deletes a machine from the cluster",
	Run: func(cmd *cobra.Command, args []string) {
		ip := cmd.Flag("ip").Value.String()

		targetMachine, err := state.ClusterClient.ClusterV1alpha1().Machines(common.DefaultNamespace).Get(ip, metav1.GetOptions{})
		if err != nil {
			log.Fatalf("Unable to get machine %q: %v", ip, err)
		}
		targetMachineSpec, err := sputil.GetMachineSpec(*targetMachine)
		if err != nil {
			log.Fatalf("Unable to decode machine %q spec: %v", targetMachine.Name, err)
		}
		targetProvisionedMachine, err := state.SPClient.SshproviderV1alpha1().ProvisionedMachines(common.DefaultNamespace).Get(targetMachineSpec.ProvisionedMachineName, metav1.GetOptions{})
		if err != nil {
			log.Fatalf("Unable to get provisioned machine %q: %v", targetMachineSpec.ProvisionedMachineName, err)
		}
		cluster, err := state.ClusterClient.ClusterV1alpha1().Clusters(common.DefaultNamespace).Get(common.DefaultClusterName, metav1.GetOptions{})
		if err != nil {
			log.Fatalf("Unable to get cluster: %v", err)
		}

		deleteMustNotOrphanNodes(targetMachine)

		if err := drainAndDeleteNodeForMachine(targetMachine, targetProvisionedMachine); err != nil {
			log.Fatalf("Unable to drain and delete cluster node for machine %q: %v", targetMachine.Name, err)
		}

		var insecureIgnoreHostKey bool = false
		if len(targetProvisionedMachine.Spec.SSHConfig.PublicKeys) == 0 {
			insecureIgnoreHostKey = true
			log.Printf("Not able to verify machine SSH identity: No public keys given. Continuing...")
		}
		machineClientBuilder := sshmachine.NewClient
		actuator := machineActuator.NewActuator(
			state.KubeClient,
			state.ClusterClient,
			state.SPClient,
			machineClientBuilder,
			insecureIgnoreHostKey,
		)
		log.Println("Deleting machine")
		if err = actuator.Delete(cluster, targetMachine); err != nil {
			log.Fatalf("Unable to delete machine: %v", err)
		}

		log.Println("Updating cluster status")
		machineStatus, err := sputil.GetMachineStatus(*targetMachine)
		if err != nil {
			log.Fatalf("Unable to get machine %q status: %v", targetMachine.Name, err)
		}
		if machineStatus.EtcdMember != nil {
			clusterStatus, err := sputil.GetClusterStatus(*cluster)
			if err != nil {
				log.Fatalf("Unable to get cluster status: %v", err)
			}

			etcdMemberSet := setsutil.NewEtcdMemberSet(clusterStatus.EtcdMembers...)
			etcdMemberSet.Delete(*machineStatus.EtcdMember)
			clusterStatus.EtcdMembers = etcdMemberSet.List()

			if err := sputil.PutClusterStatus(*clusterStatus, cluster); err != nil {
				log.Fatalf("Unable to update cluster status: %v", err)
			}
			if _, err := state.ClusterClient.ClusterV1alpha1().Clusters(common.DefaultNamespace).UpdateStatus(cluster); err != nil {
				log.Fatalf("Unable to update cluster: %v", err)
			}
		}

		if err := state.ClusterClient.ClusterV1alpha1().Machines(common.DefaultNamespace).Delete(targetMachine.Name, &metav1.DeleteOptions{}); err != nil {
			log.Fatalf("unable to delete machine %q: %v", targetMachine.Name, err)
		}
		if err := state.SPClient.SshproviderV1alpha1().ProvisionedMachines(common.DefaultNamespace).Delete(targetProvisionedMachine.Name, &metav1.DeleteOptions{}); err != nil {
			log.Fatalf("unable to delete provisioned machine %q: %v", targetProvisionedMachine.Name, err)
		}

		if err := state.PullFromAPIs(); err != nil {
			log.Fatalf("Unable to sync on-disk state: %v", err)
		}

		log.Println("Machine deleted successfully.")
	},
}

func deleteMustNotOrphanNodes(targetMachine *clusterv1.Machine) {
	if clusterutil.RoleContains(clustercommon.MasterRole, targetMachine.Spec.Roles) {
		machineList, err := state.ClusterClient.ClusterV1alpha1().Machines(common.DefaultNamespace).List(metav1.ListOptions{})
		if err != nil {
			log.Fatalf("Unable to list machines: %v", err)
		}
		countMasters := 0
		countNodes := 0
		for _, machine := range machineList.Items {
			for _, role := range machine.Spec.Roles {
				switch role {
				case clustercommon.MasterRole:
					countMasters++
				case clustercommon.NodeRole:
					countNodes++
				}
			}
		}
		if countMasters == 1 && countNodes > 0 {
			log.Fatalf("Not deleting last master while %v nodes are in the cluster. Delete the nodes first.", countNodes)
		}
	}
}

func bootstrapTokenSecretFromMachine(machine *clusterv1.Machine, provisionedMachine *spv1.ProvisionedMachine) (*corev1.Secret, error) {
	machineClient, err := sshMachineClientFromSSHConfig(provisionedMachine.Spec.SSHConfig)
	if err != nil {
		return nil, fmt.Errorf("unable to create machine client for machine %q: %v", machine.Name, err)
	}
	cmd := "/opt/bin/kubeadm token create --print-join-command"
	stdOut, stdErr, err := machineClient.RunCommand(cmd)
	if err != nil {
		return nil, fmt.Errorf("error running %q: %v (%s) (%s)", cmd, err, string(stdOut), string(stdErr))
	}
	token, caHash, err := tokenAndCAHashFromKubeadmJoinCommand(string(stdOut))
	if err != nil {
		return nil, fmt.Errorf("unable to parse bootstrap token from stdout of %q: %q", cmd, stdOut)
	}
	secret := corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Secret",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              common.DefaultBootstrapTokenSecretName,
			Namespace:         common.DefaultNamespace,
			CreationTimestamp: metav1.Now(),
		},
		Data: map[string][]byte{
			"token":  []byte(token),
			"cahash": []byte(caHash),
		},
	}
	return &secret, nil
}

func masterMachineAndProvisionedMachine() (*clusterv1.Machine, *spv1.ProvisionedMachine, error) {
	machineList, err := state.ClusterClient.ClusterV1alpha1().Machines(common.DefaultNamespace).List(metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("unable to list machines: %v", err)
	}
	var masterMachine *clusterv1.Machine
	for _, machine := range machineList.Items {
		if clusterutil.RoleContains(clustercommon.MasterRole, machine.Spec.Roles) {
			// Choose first master in the list
			masterMachine = machine.DeepCopy()
			break
		}
	}
	if masterMachine == nil {
		return nil, nil, fmt.Errorf("unable to find any machine with Master role, cannot obtain bootstrap token")
	}
	masterMachineSpec, err := sputil.GetMachineSpec(*masterMachine)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to decode machine spec: %v", err)
	}
	masterProvisionedMachine, err := state.SPClient.SshproviderV1alpha1().ProvisionedMachines(common.DefaultNamespace).Get(masterMachineSpec.ProvisionedMachineName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("unable to get provisioned machine: %v", err)
	}
	return masterMachine, masterProvisionedMachine.DeepCopy(), nil
}

func tokenAndCAHashFromKubeadmJoinCommand(cmdStdout string) (string, string, error) {
	fields := strings.Fields(cmdStdout)
	//Successful output would be of the type
	//kubeadm join <server:port> --token <token> --discovery-token-ca-cert-hash <sha>
	if len(fields) != 7 { //TODO(puneet) Needs a better way but seems good-enough for now
		return "", "", fmt.Errorf("expected 7 fields, found %v", len(fields))
	}
	token := fields[4]
	caHash := fields[6]
	return token, caHash, nil
}

func adminKubeconfigFromMachine(machine *clusterv1.Machine, provisionedMachine *spv1.ProvisionedMachine) ([]byte, error) {
	machineClient, err := sshMachineClientFromSSHConfig(provisionedMachine.Spec.SSHConfig)
	if err != nil {
		return nil, fmt.Errorf("unable to create machine client for machine %q: %v", machine.Name, err)
	}
	return machineClient.ReadFile("/etc/kubernetes/admin.conf")
}

func writeAdminKubeconfigToMachine(kubeconfig []byte, machine *clusterv1.Machine, provisionedMachine *spv1.ProvisionedMachine) error {
	machineClient, err := sshMachineClientFromSSHConfig(provisionedMachine.Spec.SSHConfig)
	if err != nil {
		return fmt.Errorf("unable to create machine client for machine %q: %v", machine.Name, err)
	}
	return machineClient.WriteFile("/etc/kubernetes/admin.conf", 0600, kubeconfig)
}

func drainAndDeleteNodeForMachine(targetMachine *clusterv1.Machine, targetProvisionedMachine *spv1.ProvisionedMachine) error {
	targetMachineClient, err := sshMachineClientFromSSHConfig(targetProvisionedMachine.Spec.SSHConfig)
	if err != nil {
		return fmt.Errorf("unable to create machine client for machine %q: %v", targetMachine.Name, err)
	}

	// TODO(dlipovetsky) Handle /opt/bin/kubectl not found. Possibly infer
	// that the nodeadm reset ran at least as far as removing the kubectl
	// binary. nodeName includes the object kind, i.e.,

	// "node/the-name-of-the-node"
	stdOut, stdErr, err := targetMachineClient.RunCommand("/opt/bin/kubectl --kubeconfig=/etc/kubernetes/admin.conf get node --selector kubernetes.io/hostname=$(hostname -f) -oname")
	if err != nil {
		return fmt.Errorf("unable to identify the cluster node: %v (%s) (%s)", err, string(stdOut), string(stdErr))
	}
	nodeName := strings.TrimSpace(string(stdOut))
	if len(nodeName) != 0 {
		log.Printf("Draining cluster node %q for machine %q", nodeName, targetMachine.Name)
		// --ignore-daemonsets is used because critical components (kube-proxy, overlay network) run as daemonsets
		// --delete-local-data is NOT used; pods using emptyDir volumes must be removed by the user, since removing them causes the data to be lost
		// --force is NOT used; unmanaged pods must be removed by the user, since they won't be rescheduled to another node
		stdOut, stdErr, err = targetMachineClient.RunCommand(fmt.Sprintf("/opt/bin/kubectl --kubeconfig=/etc/kubernetes/admin.conf drain --timeout=%v --grace-period=%v --ignore-daemonsets %v", drainTimeout, drainGracePeriodSeconds, nodeName))
		if err != nil {
			return fmt.Errorf("unable to drain cluster node %q: %v (%s) (%s)", nodeName, err, string(stdOut), string(stdErr))
		}
		log.Println(string(stdOut))

		log.Printf("Deleting cluster node %q for machine %q", nodeName, targetMachine.Name)
		stdOut, stdErr, err = targetMachineClient.RunCommand(fmt.Sprintf("/opt/bin/kubectl --kubeconfig=/etc/kubernetes/admin.conf delete %v", nodeName))
		if err != nil {
			return fmt.Errorf("unable to delete cluster node %q: %v (%s) (%s)", nodeName, err, string(stdOut), string(stdErr))
		}
		log.Println(string(stdOut))
	}
	return nil
}

func sshMachineClientFromSSHConfig(sshConfig *spv1.SSHConfig) (sshmachine.Client, error) {
	sshCredentialSecret, err := state.KubeClient.CoreV1().Secrets(common.DefaultNamespace).Get(sshConfig.CredentialSecret.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("unable to find SSH credential %q", sshConfig.CredentialSecret.Name)
		}
		return nil, fmt.Errorf("unable to get SSH credential secret: %v", err)
	}
	username, privateKey, err := sputil.UsernameAndKeyFromSecret(sshCredentialSecret)
	if err != nil {
		return nil, fmt.Errorf("unable to read SSH credential from secret: %v", err)
	}
	var insecureIgnoreHostKey bool
	if len(sshConfig.PublicKeys) == 0 {
		insecureIgnoreHostKey = true
		log.Printf("Not able to verify machine SSH identity: No public keys given. Continuing...")
	}
	return sshmachine.NewClient(sshConfig.Host, sshConfig.Port, username, privateKey, sshConfig.PublicKeys, insecureIgnoreHostKey)
}

var machineCmdGet = &cobra.Command{
	Use:   "machine",
	Short: "Get machine resources",
	Run: func(cmd *cobra.Command, args []string) {
		ip := cmd.Flag("ip").Value.String()
		var machineList *clusterv1.MachineList
		if len(ip) == 0 {
			var err error
			machineList, err = state.ClusterClient.ClusterV1alpha1().Machines(common.DefaultNamespace).List(metav1.ListOptions{})
			if err != nil {
				log.Fatalf("Unable to list machines: %v", err)
			}
		} else {
			machine, err := state.ClusterClient.ClusterV1alpha1().Machines(common.DefaultNamespace).Get(ip, metav1.GetOptions{})
			if err != nil {
				log.Fatalf("Unable to get machine %q: %v", ip, err)
			}
			machineList = &clusterv1.MachineList{
				Items: []clusterv1.Machine{*machine},
			}
		}
		switch outputFmt {
		case "yaml":
			bytes, err := yaml.Marshal(machineList.Items)
			if err != nil {
				log.Fatalf("Unable to marshal cluster spec file to yaml: %s", err)
			}
			os.Stdout.Write(bytes)
		case "json":
			bytes, err := json.Marshal(machineList.Items)
			if err != nil {
				log.Fatalf("Unable to marshal cluster spec file to json: %s", err)
			}
			os.Stdout.Write(bytes)
		case "":
			t := template.Must(template.New("MachineV1PrintTemplate").Parse(common.MachineV1PrintTemplate))
			if err := t.Execute(os.Stdout, machineList.Items); err != nil {
				log.Fatalf("Could not pretty print cluster details: %s", err)
			}
		default:
			log.Fatalf("Unsupported output format %q", outputFmt)
		}
	},
}

func init() {
	createCmd.AddCommand(machineCmdCreate)
	machineCmdCreate.Flags().String("ip", "", "IP of the machine")
	machineCmdCreate.Flags().Int("port", common.DefaultSSHPort, "SSH port")
	machineCmdCreate.Flags().String("role", "", "Role of the machine. Can be master/node")
	machineCmdCreate.Flags().String("publicKeys", "", "Comma separated list of public host keys for the machine")
	machineCmdCreate.Flags().String("sshSecretName", "sshSecret", "Name of the sshSecret to use")
	machineCmdCreate.Flags().String("iface", "eth0", "Interface that keepalived will bind to in case of master")

	deleteCmd.AddCommand(machineCmdDelete)
	machineCmdDelete.Flags().String("ip", "", "IP of the machine")
	machineCmdDelete.Flags().String("force", "", "Force delete the machine")
	machineCmdDelete.Flags().DurationVar(&drainTimeout, "drain-timeout", common.DRAIN_TIMEOUT, "The length of time to wait before giving up, zero means infinite")
	machineCmdDelete.Flags().IntVar(&drainGracePeriodSeconds, "drain-graceperiod", common.DRAIN_GRACE_PERIOD_SECONDS, "Period of time in seconds given to each pod to terminate gracefully. If negative, the default value specified in the pod will be used.")

	machineCmdGet.Flags().String("ip", "", "IP of the machine")
	getCmd.AddCommand(machineCmdGet)
}