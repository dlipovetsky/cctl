package cmd

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clustercommon "sigs.k8s.io/cluster-api/pkg/apis/cluster/common"
	clusterv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"

	"github.com/platform9/cctl/common"
	spv1 "github.com/platform9/ssh-provider/pkg/apis/sshprovider/v1alpha1"
	sputil "github.com/platform9/ssh-provider/pkg/controller"
	sshmachine "github.com/platform9/ssh-provider/pkg/machine"
	setsutil "github.com/platform9/ssh-provider/pkg/util/sets"
	"github.com/spf13/cobra"
)

var recoverEtcdCmd = &cobra.Command{
	Use:   "etcd",
	Short: "Recovers the etcd cluster from a snapshot",
	Run: func(cmd *cobra.Command, args []string) {
		localPath, err := cmd.Flags().GetString("snapshot")
		if err != nil {
			log.Fatalf("Unable to parse `snapshot`: %v", err)
		}
		remotePath := common.EtcdSnapshotRemotePath

		cluster, err := state.ClusterClient.ClusterV1alpha1().Clusters(common.DefaultNamespace).Get(common.DefaultClusterName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				log.Fatalf("No cluster found. Create a cluster before creating a machine.")
			}
			log.Fatalf("Unable to get cluster: %v", err)
		}
		clusterProviderSpec, err := sputil.GetClusterSpec(*cluster)
		if err != nil {
			log.Fatalf("Unable to decode cluster spec: %v", err)
		}
		etcdCASecret, err := state.KubeClient.CoreV1().Secrets(common.DefaultNamespace).Get(clusterProviderSpec.EtcdCASecret.Name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				log.Fatalf("Unable to get etcd CA secret: %v", err)
			}
		}

		var masters []*clusterv1.Machine
		machineList, err := state.ClusterClient.ClusterV1alpha1().Machines(common.DefaultNamespace).List(metav1.ListOptions{})
		if err != nil {
			log.Fatalf("Unable to list machines: %v", err)
		}
		for _, machine := range machineList.Items {
			for _, role := range machine.Spec.Roles {
				if role == clustercommon.MasterRole {
					log.Printf("[recover etcd] Found master %q", machine.Name)
					masters = append(masters, machine.DeepCopy())
				}
			}
		}

		if err := recoverEtcd(localPath, remotePath, etcdCASecret, cluster, masters); err != nil {
			log.Fatalf("Unable to recover etcd: %v", err)
		}

		if err := state.PullFromAPIs(); err != nil {
			log.Fatalf("Unable to sync on-disk state: %v", err)
		}

		log.Println("Recovered etcd successfully.")
	},
}

func recoverEtcd(localPath, remotePath string, etcdCASecret *corev1.Secret, cluster *clusterv1.Cluster, masters []*clusterv1.Machine) error {
	if len(masters) == 0 {
		return nil
	}

	mastersWithClient := make([]struct {
		Machine *clusterv1.Machine
		Client  sshmachine.Client
	}, len(masters))
	for i, master := range masters {
		machineStatus, err := sputil.GetMachineStatus(*master)
		if err != nil {
			return fmt.Errorf("unable to decode machine %q spec: %v", master.Name, err)
		}
		client, err := sshMachineClientFromSSHConfig(machineStatus.SSHConfig)
		if err != nil {
			return fmt.Errorf("unable to create machine client for machine %q: %v", master.Name, err)
		}
		mastersWithClient[i].Machine = master
		mastersWithClient[i].Client = client
	}

	// Reset all masters
	log.Println("[recover etcd] Cleaning up degraded etcd cluster on all masters")
	for _, mwc := range mastersWithClient {
		if err := resetEtcdSkipRemoveMember(mwc.Client); err != nil {
			return fmt.Errorf("unable to reset etcd on machine %q: %v", mwc.Machine.Name, err)
		}

		machineStatus, err := sputil.GetMachineStatus(*mwc.Machine)
		if err != nil {
			return fmt.Errorf("unable to decode machine status: %v", err)
		}
		if err := removeClusterEtcdMember(*machineStatus.EtcdMember, cluster); err != nil {
			return fmt.Errorf("unable to remove etcd member information for machine %q from cluster status: %v", mwc.Machine.Name, err)
		}
	}

	// Write etcd CA to all masters
	log.Println("[recover etcd] Writing etcd CA to all masters")
	for _, mwc := range mastersWithClient {
		if err := writeSecretToMachine(mwc.Client, etcdCASecret, "tls.crt", "tls.key", "/etc/etcd/pki/ca.crt", "/etc/etcd/pki/ca.key"); err != nil {
			return fmt.Errorf("unable to write etcd CA cert and key to machine %q: %v", mwc.Machine.Name, err)
		}
	}

	firstMWC := mastersWithClient[0]
	otherMWCs := mastersWithClient[1:]

	// Recover the first master
	log.Printf("[recover etcd] Initializing new etcd cluster from snapshot on master %q", firstMWC.Machine.Name)
	if err := writeEtcdSnapshot(localPath, remotePath, firstMWC.Client); err != nil {
		return fmt.Errorf("unable to write etcd snapshot to machine %q: %v", firstMWC.Machine.Name, err)
	}
	if err := etcdadmInitFromSnapshot(remotePath, firstMWC.Client); err != nil {
		return fmt.Errorf("error running etcdadm init on machine %q: %v", firstMWC.Machine.Name, err)
	}
	firstEtcdMember, err := etcdMemberFromMachine(firstMWC.Client)
	if err != nil {
		return fmt.Errorf("error reading etcd member data from machine %q: %v", firstMWC.Machine.Name, err)
	}
	if err := updateMachineEtcdMember(firstEtcdMember, firstMWC.Machine); err != nil {
		return fmt.Errorf("unable to update machine %q status with etcd member %q: %v", firstMWC.Machine.Name, firstEtcdMember, err)
	}
	if err := insertClusterEtcdMember(firstEtcdMember, cluster); err != nil {
		return fmt.Errorf("unable to update cluster status with etcd member %q: %v", firstEtcdMember, err)
	}

	// Recover the other masters
	if len(firstEtcdMember.ClientURLs) == 0 {
		return fmt.Errorf("unable to proceed: etcd member for machine %q has no client URLs", firstMWC.Machine.Name)
	}
	endpoint := firstEtcdMember.ClientURLs[0]
	for _, mwc := range otherMWCs {
		log.Printf("[recover etcd] Joining master %q to new etcd cluster", mwc.Machine.Name)
		if err := etcdadmJoin(endpoint, mwc.Client); err != nil {
			return fmt.Errorf("error running etcdadm join on machine %q: %v", mwc.Machine.Name, err)
		}
		etcdMember, err := etcdMemberFromMachine(mwc.Client)
		if err != nil {
			return fmt.Errorf("error reading etcd member data from machine %q: %v", mwc.Machine.Name, err)
		}
		if err := updateMachineEtcdMember(etcdMember, mwc.Machine); err != nil {
			return fmt.Errorf("unable to update machine %q status with etcd member %q: %v", mwc.Machine.Name, etcdMember, err)
		}
		if err := insertClusterEtcdMember(etcdMember, cluster); err != nil {
			return fmt.Errorf("unable to update cluster status with etcd member %q: %v", etcdMember, err)
		}
	}

	for _, mwc := range mastersWithClient {
		log.Printf("[recover etcd] Restarting kubelet on master %q to trigger immediate API server restart", mwc.Machine.Name)
		if err := restartKubelet(mwc.Client); err != nil {
			log.Printf("Warning: Unable to restart kubelet on machine %q: %v", mwc.Machine.Name, err)
		}
	}

	return nil
}

func updateMachineEtcdMember(etcdMember spv1.EtcdMember, machine *clusterv1.Machine) error {
	machineStatus, err := sputil.GetMachineStatus(*machine)
	if err != nil {
		return fmt.Errorf("unable to decode machine status: %v", err)
	}
	machineStatus.EtcdMember = &etcdMember
	if err := sputil.PutMachineStatus(*machineStatus, machine); err != nil {
		return fmt.Errorf("unable to encode machine status: %v", err)
	}
	if _, err := state.ClusterClient.ClusterV1alpha1().Machines(machine.Namespace).UpdateStatus(machine); err != nil {
		return fmt.Errorf("error updating machine %q: %v", machine.Name, err)
	}
	return nil
}

func insertClusterEtcdMember(etcdMember spv1.EtcdMember, cluster *clusterv1.Cluster) error {
	clusterStatus, err := sputil.GetClusterStatus(*cluster)
	if err != nil {
		return fmt.Errorf("unable to decode cluster status: %v", err)
	}
	etcdMemberSet := setsutil.NewEtcdMemberSet(clusterStatus.EtcdMembers...)
	etcdMemberSet.Insert(etcdMember)
	clusterStatus.EtcdMembers = etcdMemberSet.List()
	if err := sputil.PutClusterStatus(*clusterStatus, cluster); err != nil {
		return fmt.Errorf("unable to encode cluster status: %v", err)
	}
	if _, err := state.ClusterClient.ClusterV1alpha1().Clusters(common.DefaultNamespace).UpdateStatus(cluster); err != nil {
		return fmt.Errorf("unable to update cluster: %v", err)
	}
	return nil
}

func removeClusterEtcdMember(etcdMember spv1.EtcdMember, cluster *clusterv1.Cluster) error {
	clusterStatus, err := sputil.GetClusterStatus(*cluster)
	if err != nil {
		return fmt.Errorf("unable to decode cluster status: %v", err)
	}
	etcdMemberSet := setsutil.NewEtcdMemberSet(clusterStatus.EtcdMembers...)
	etcdMemberSet.Delete(etcdMember)
	clusterStatus.EtcdMembers = etcdMemberSet.List()
	if err := sputil.PutClusterStatus(*clusterStatus, cluster); err != nil {
		return fmt.Errorf("unable to encode cluster status: %v", err)
	}
	if _, err := state.ClusterClient.ClusterV1alpha1().Clusters(common.DefaultNamespace).UpdateStatus(cluster); err != nil {
		return fmt.Errorf("unable to update cluster: %v", err)
	}
	return nil
}

func resetEtcdSkipRemoveMember(client sshmachine.Client) error {
	cmd := fmt.Sprintf("%s reset --skip-remove-member", "/opt/bin/etcdadm")
	stdOut, stdErr, err := client.RunCommand(cmd)
	if err != nil {
		return fmt.Errorf("error running %q: %v (stdout: %q, stderr: %q)", cmd, err, string(stdOut), string(stdErr))
	}
	return nil
}

func writeEtcdSnapshot(localPath, remotePath string, client sshmachine.Client) error {
	b, err := ioutil.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("unable to read etcd snapshot %q: %v", localPath, err)
	}
	return client.WriteFile(remotePath, 0600, b)
}

func etcdadmInitFromSnapshot(remotePath string, client sshmachine.Client) error {
	cmd := fmt.Sprintf("%s init --snapshot %s", "/opt/bin/etcdadm", remotePath)
	stdOut, stdErr, err := client.RunCommand(cmd)
	if err != nil {
		return fmt.Errorf("error running %q: %v (stdout: %q, stderr: %q)", cmd, err, string(stdOut), string(stdErr))
	}
	return nil
}

func etcdadmJoin(endpoint string, client sshmachine.Client) error {
	cmd := fmt.Sprintf("%s join %s", "/opt/bin/etcdadm", endpoint)
	stdOut, stdErr, err := client.RunCommand(cmd)
	if err != nil {
		return fmt.Errorf("error running %q: %v (stdout: %q, stderr: %q)", cmd, err, string(stdOut), string(stdErr))
	}
	return nil
}

// writeSecretToMachine is a near copy of the function in
// https://github.com/platform9/ssh-provider/blob/28922e78090ea51444156996f70d5236f4ddc256/pkg/clusterapi/machine/secrets.go#L88
// TODO(dlipovetsky) Once this code is moved out of the actuator and exported,
// import it and remove this function.
func writeSecretToMachine(machineClient sshmachine.Client, secret *corev1.Secret, certKey, keyKey, certPath, keyPath string) error {
	cert, ok := secret.Data[certKey]
	if !ok {
		return fmt.Errorf("did not find key %q in secret %q", certKey, secret.Name)
	}
	key, ok := secret.Data[keyKey]
	if !ok {
		return fmt.Errorf("did not find key %q in secret %q", keyKey, secret.Name)
	}
	// TODO(dlipovetsky) Use same dir for cert and key
	certDir := filepath.Dir(certPath)
	if err := machineClient.MkdirAll(certDir, 0755); err != nil {
		return fmt.Errorf("unable to create cert dir %q on machine: %v", certDir, err)
	}
	keyDir := filepath.Dir(keyPath)
	if err := machineClient.MkdirAll(keyDir, 0755); err != nil {
		return fmt.Errorf("unable to create key dir %q on machine: %v", keyDir, err)
	}

	// Non root users will not have permission to write to /etc/ directly
	// Write cert and key to /tmp instead and then move the certs over to their respective paths
	tmpCertPath := fmt.Sprintf("/tmp/%s", certKey)
	tmpKeyPath := fmt.Sprintf("/tmp/%s", keyKey)
	if err := machineClient.WriteFile(tmpCertPath, 0644, cert); err != nil {
		return fmt.Errorf("unable to write cert to %q on machine: %v", tmpCertPath, err)
	}
	if err := machineClient.WriteFile(tmpKeyPath, 0600, key); err != nil {
		return fmt.Errorf("unable to write key to %q on machine: %v", tmpKeyPath, err)
	}
	// Copy cert and key from /tmp to its respective destination
	if err := machineClient.MoveFile(tmpCertPath, certPath); err != nil {
		return err
	}
	return machineClient.MoveFile(tmpKeyPath, keyPath)
}

// etcdMemberFromMachine is near copy of the function in
// https://github.com/platform9/ssh-provider/blob/28922e78090ea51444156996f70d5236f4ddc256/pkg/clusterapi/machine/master.go#L46
// TODO(dlipovetsky) Once this code is moved out of the actuator and exported,
// import it and remove this function.
func etcdMemberFromMachine(machineClient sshmachine.Client) (spv1.EtcdMember, error) {
	var etcdMember spv1.EtcdMember
	cmd := fmt.Sprintf("%s info", "/opt/bin/etcdadm")
	stdOut, stdErr, err := machineClient.RunCommand(cmd)
	if err != nil {
		return etcdMember, fmt.Errorf("error running %q: %v (stdout: %q, stderr: %q)", cmd, err, string(stdOut), string(stdErr))
	}
	err = json.Unmarshal(stdOut, &etcdMember)
	if err != nil {
		return etcdMember, fmt.Errorf("error unmarshalling etcdadm info output: %v", err)
	}
	return etcdMember, nil
}

func restartKubelet(client sshmachine.Client) error {
	cmd := fmt.Sprintf("systemctl restart kubelet")
	stdOut, stdErr, err := client.RunCommand(cmd)
	if err != nil {
		return fmt.Errorf("error running %q: %v (stdout: %q, stderr: %q)", cmd, err, string(stdOut), string(stdErr))
	}
	return nil
}

func init() {
	recoverEtcdCmd.Flags().String("snapshot", "", "Path of the etcd snapshot used to recover the cluster.")
	recoverCmd.AddCommand(recoverEtcdCmd)
}