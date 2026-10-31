{ config, pkgs, ... }:

let
  maelleProxy = pkgs.buildGoModule {
    pname = "maelle-proxy";
    version = "0.2.0";
    src = ./maelle-proxy;
    vendorHash = null;
  };
in
{
  imports = [ ./hardware-configuration.nix ];

  # Boot
  boot.loader.grub = {
    enable = true;
    version = 2;
    device = "@GRUB_DEVICE@";
    efiSupport = @EFI_SUPPORT@;
  };
  boot.loader.efi.canTouchEfiVariables = @EFI_CAN_TOUCH@;

  boot.kernelParams = [ "console=tty0" "console=ttyS0,115200" ];
  boot.loader.grub.extraConfig = ''
    serial --speed=115200 --unit=0 --word=8 --parity=no --stop=1
    terminal_input serial console
    terminal_output serial console
  '';

  # Networking
  networking.hostName = "machine";
  networking.useDHCP = true;
  networking.firewall.enable = true;
  networking.firewall.allowedTCPPorts = [ 80 443 ];

  # SSH
  services.openssh = {
    enable = true;
    settings = {
      PermitRootLogin = "no";
      PasswordAuthentication = false;
    };
  };
  users.users.maelle = {
    isNormalUser = true;
    group = "maelle";
    extraGroups = [ "wheel" ];
    openssh.authorizedKeys.keys = [ "@SSH_PUB@" ];
  };
  users.groups.maelle = { };
  security.sudo.wheelNeedsPassword = false;

  # Nginx
  security.acme = {
    acceptTerms = true;
    defaults.email = "venomade@venomade.com";
  };

  services.nginx = {
    enable = true;
    recommendedGzipSettings = true;
    recommendedOptimisation = true;
    recommendedProxySettings = true;
    recommendedTlsSettings = true;
    virtualHosts."maelle.sh" = {
      enableACME = true;
      forceSSL = true;
      root = "/etc/nixos/maelle-site";
    };
    virtualHosts."api.maelle.sh" = {
      enableACME = true;
      forceSSL = true;
      locations."/".proxyPass = "http://127.0.0.1:8080";
      extraConfig = ''
        proxy_buffering off;
        proxy_read_timeout 3600s;
      '';
    };
  };

  # Maelle Proxy
  systemd.services.maelle-proxy = {
    description = "Maelle — OpenAI-compatible proxy with server-side tools";
    after = [ "network-online.target" ];
    wants = [ "network-online.target" ];
    wantedBy = [ "multi-user.target" ];
    environment = {
      MAELLE_LISTEN = "127.0.0.1:8080";
      MAELLE_PERSONA = "/etc/nixos/maelle-proxy/persona.md";
      MAELLE_TOOLS = "/etc/nixos/maelle-proxy/tools.json";
    };
    serviceConfig = {
      ExecStart = "${maelleProxy}/bin/maelle-proxy";
      User = "maelle";
      Group = "maelle";
      EnvironmentFile = "/etc/maelle.env";
      Restart = "always";
      RestartSec = "3s";
    };
  };

  services.qemuGuest.enable = true;    # Hetzner runs KVM/QEMU

  environment.systemPackages = with pkgs; [
    git
  ];

  nix.settings.experimental-features = [ "nix-command" "flakes" ];

  system.stateVersion = "26.05";
}
