# OIDC issuer for the standalone stage authority: the Optimi `oauthsonas` persona provider (the same issuer the
# local checkpoints use), pinned by digest. It is a development/test fixture: persona selection replaces
# authentication, so anyone who completes its authorize flow can act as any persona. The stage therefore
#   - gates the authorize flow with a long random access code (OAUTHSONAS_ACCESS_CODE, from a Secret),
#   - restricts /oauth2, /userinfo and /logout to operator CIDRs at the ingress (discovery and JWKS stay open
#     because the server and every client must fetch them), and
#   - runs one replica with Recreate (all state, including the signing key, is in memory).
# Recorded as O-27; hosted authority (T-061) and T-106 replace it.

locals {
  issuer_url   = coalesce(var.oidc_issuer, "https://auth.${local.deps_host}")
  issuer_host  = trimprefix(local.issuer_url, "https://")
  api_audience = coalesce(var.oidc_audience, "https://${local.deps_host}/api")

  persona_roles = {
    admin    = ["admin"]
    analyst  = ["analyst"]
    engineer = ["engineer"]
    reviewer = ["reviewer"]
    viewer   = ["viewer"]
    outsider = ["admin", "reviewer"] # claims galore, bindings none
    foreign  = ["engineer"]
  }

  oauthsonas_config = jsonencode({
    issuer                 = local.issuer_url
    api_audience           = local.api_audience
    token_ttl              = "15m"
    authorization_code_ttl = "5m"
    refresh_token_ttl      = "8h"
    clients = [{
      id                        = "kodama-cli"
      name                      = "Kodama CLI"
      public                    = true
      redirect_uris             = ["http://127.0.0.1:18999/callback"]
      post_logout_redirect_uris = ["http://127.0.0.1:18999/"]
      allowed_origins           = ["http://127.0.0.1:18999"]
    }]
    personas = [for p in local.personas : {
      id      = p
      subject = "oauthsonas|${p}"
      email   = "${p}@stage.kodama.invalid"
      name    = "Stage ${p}"
      roles   = local.persona_roles[p]
    }]
  })

  # The operator networks allowed to reach the token-issuing paths. The ingress sees real client addresses
  # (PROXY protocol); in-cluster callers appear as node addresses and use the Service directly instead.
  issuer_allowlist = join(",", var.issuer_allowed_cidrs)
}

resource "random_password" "oauthsonas_access_code" {
  length  = 40
  special = false
}

resource "kubernetes_secret_v1" "oauthsonas_access" {
  metadata {
    name      = "kodama-oauthsonas-access"
    namespace = kubernetes_namespace_v1.deps.metadata[0].name
  }
  data = {
    "access-code" = random_password.oauthsonas_access_code.result
  }
}

resource "kubernetes_config_map_v1" "oauthsonas" {
  metadata {
    name      = "kodama-oauthsonas"
    namespace = kubernetes_namespace_v1.deps.metadata[0].name
  }
  data = {
    "oauthsonas.json" = local.oauthsonas_config
  }
}

resource "kubernetes_deployment_v1" "oauthsonas" {
  metadata {
    name      = "kodama-oauthsonas"
    namespace = kubernetes_namespace_v1.deps.metadata[0].name
    labels    = { "app.kubernetes.io/name" = "oauthsonas" }
  }

  spec {
    replicas = 1
    strategy {
      type = "Recreate"
    }
    selector {
      match_labels = { "app.kubernetes.io/name" = "oauthsonas" }
    }

    template {
      metadata {
        labels = { "app.kubernetes.io/name" = "oauthsonas" }
        annotations = {
          "checksum/config" = sha256(local.oauthsonas_config)
        }
      }

      spec {
        automount_service_account_token = false
        enable_service_links            = false

        security_context {
          run_as_non_root = true
          run_as_user     = 65532
          run_as_group    = 65532
          seccomp_profile {
            type = "RuntimeDefault"
          }
        }

        container {
          name  = "oauthsonas"
          image = var.oauthsonas_image
          args  = ["--config", "/etc/oauthsonas/oauthsonas.json", "--listen", "0.0.0.0:8181"]

          env {
            name  = "OAUTHSONAS_ALLOW_NON_LOOPBACK"
            value = "true"
          }
          env {
            name = "OAUTHSONAS_ACCESS_CODE"
            value_from {
              secret_key_ref {
                name = kubernetes_secret_v1.oauthsonas_access.metadata[0].name
                key  = "access-code"
              }
            }
          }

          port {
            name           = "http"
            container_port = 8181
          }

          readiness_probe {
            http_get {
              path = "/readyz"
              port = "http"
            }
            period_seconds = 5
          }
          liveness_probe {
            http_get {
              path = "/healthz"
              port = "http"
            }
            period_seconds    = 20
            failure_threshold = 3
          }

          resources {
            requests = { cpu = "20m", memory = "32Mi" }
            limits   = { memory = "128Mi" }
          }

          security_context {
            allow_privilege_escalation = false
            read_only_root_filesystem  = true
            capabilities {
              drop = ["ALL"]
            }
          }

          volume_mount {
            name       = "config"
            mount_path = "/etc/oauthsonas"
            read_only  = true
          }
        }

        volume {
          name = "config"
          config_map {
            name = kubernetes_config_map_v1.oauthsonas.metadata[0].name
          }
        }
      }
    }
  }

  wait_for_rollout = true
}

resource "kubernetes_service_v1" "oauthsonas" {
  metadata {
    name      = "kodama-oauthsonas"
    namespace = kubernetes_namespace_v1.deps.metadata[0].name
  }
  spec {
    selector = { "app.kubernetes.io/name" = "oauthsonas" }
    port {
      name        = "http"
      port        = 8181
      target_port = "http"
    }
  }
}

# Public ingress for discovery, JWKS and health: the server and every client fetch these. This ingress also
# requests the Let's Encrypt certificate for the issuer host.
resource "kubernetes_ingress_v1" "oauthsonas_open" {
  metadata {
    name      = "kodama-oauthsonas"
    namespace = kubernetes_namespace_v1.deps.metadata[0].name
    annotations = {
      "cert-manager.io/cluster-issuer" = "letsencrypt"
    }
  }
  spec {
    ingress_class_name = "nginx"
    tls {
      hosts       = [local.issuer_host]
      secret_name = "kodama-oauthsonas-tls"
    }
    rule {
      host = local.issuer_host
      http {
        path {
          path      = "/"
          path_type = "Prefix"
          backend {
            service {
              name = kubernetes_service_v1.oauthsonas.metadata[0].name
              port {
                name = "http"
              }
            }
          }
        }
      }
    }
  }
  depends_on = [helm_release.cluster_issuer]
}

# The token-issuing paths, reachable only from the allowlist (longer prefixes win over the open ingress).
resource "kubernetes_ingress_v1" "oauthsonas_restricted" {
  metadata {
    name      = "kodama-oauthsonas-restricted"
    namespace = kubernetes_namespace_v1.deps.metadata[0].name
    annotations = {
      "nginx.ingress.kubernetes.io/whitelist-source-range" = local.issuer_allowlist
    }
  }
  spec {
    ingress_class_name = "nginx"
    tls {
      hosts       = [local.issuer_host]
      secret_name = "kodama-oauthsonas-tls"
    }
    rule {
      host = local.issuer_host
      http {
        dynamic "path" {
          for_each = ["/oauth2", "/userinfo", "/logout"]
          content {
            path      = path.value
            path_type = "Prefix"
            backend {
              service {
                name = kubernetes_service_v1.oauthsonas.metadata[0].name
                port {
                  name = "http"
                }
              }
            }
          }
        }
      }
    }
  }
}
