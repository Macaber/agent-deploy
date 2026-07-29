package com.example.workspace;

import com.example.workspace.operator.WorkspaceReconciler;
import io.fabric8.kubernetes.client.KubernetesClient;
import io.fabric8.kubernetes.client.KubernetesClientBuilder;
import io.javaoperatorsdk.operator.Operator;
import io.javaoperatorsdk.operator.api.config.LeaderElectionConfiguration;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.boot.SpringApplication;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.boot.autoconfigure.condition.ConditionalOnProperty;
import org.springframework.context.annotation.Bean;

@SpringBootApplication
public class WorkspaceApplication {

    public static void main(String[] args) {
        SpringApplication.run(WorkspaceApplication.class, args);
    }

    @Bean
    public KubernetesClient kubernetesClient() {
        return new KubernetesClientBuilder().build();
    }

    @Bean(destroyMethod = "stop")
    @ConditionalOnProperty(name = "app.operator.enabled", havingValue = "true", matchIfMissing = true)
    public Operator operator(KubernetesClient client,
                             WorkspaceReconciler reconciler,
                             @Value("${app.operator.leader-election.enabled:false}") boolean leaderElectionEnabled,
                             @Value("${app.operator.leader-election.lease-name:agent-deploy-operator-lease}") String leaseName) {
        Operator operator = new Operator(overrider -> {
            overrider.withKubernetesClient(client);
            if (leaderElectionEnabled) {
                overrider.withLeaderElectionConfiguration(new LeaderElectionConfiguration(leaseName));
            }
        });
        operator.register(reconciler);
        operator.start();
        return operator;
    }
}
