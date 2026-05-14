pipeline {
  agent any

  options {
    timestamps()
    disableConcurrentBuilds()
    buildDiscarder(logRotator(numToKeepStr: '20'))
  }

  environment {
    CGO_ENABLED = '0'
    GOPROXY = 'https://goproxy.cn,direct'
    GOFLAGS = '-trimpath'
    PATH = "/var/data/go/1.24.3/go/bin:${env.PATH}"
    DEPLOY_DIR = '/var/www/slp/sp-mcp'
    SUPERVISOR_CONF = '/etc/supervisor/conf.d/sp-mcp.conf'
    SERVICE_NAME = 'sp-mcp'
    SUDO = 'sudo -n'
    INSTALL = '/usr/bin/install'
    SUPERVISORCTL = '/usr/bin/supervisorctl'
  }

  stages {
    stage('Checkout') {
      steps {
        checkout scm
      }
    }

    stage('Setup Go') {
      steps {
        sh '''
          go env -w GOPROXY=https://goproxy.cn,direct
          go env GOPROXY
        '''
      }
    }

    stage('Go Version') {
      steps {
        sh 'go version'
      }
    }

    stage('Test') {
      steps {
        sh 'go test ./...'
      }
    }

    stage('Build') {
      steps {
        sh '''
          mkdir -p dist
          go build -ldflags="-s -w" -o dist/sp-mcp ./cmd/sp-mcp
          cp config.example.json dist/config.example.json
          cp deploy/supervisor/sp-mcp.conf dist/sp-mcp.supervisor.conf
        '''
      }
    }

    stage('Smoke Test') {
      steps {
        sh '''
          printf '%s\n' \
            '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
            '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' \
            | ./dist/sp-mcp -config config.example.json
        '''
      }
    }

    stage('Deploy') {
      steps {
        sh '''
          $SUDO $INSTALL -d -m 0755 "$DEPLOY_DIR"
          $SUDO $INSTALL -m 0755 dist/sp-mcp "$DEPLOY_DIR/sp-mcp"
          $SUDO $INSTALL -m 0644 dist/config.example.json "$DEPLOY_DIR/config.example.json"
          if [ ! -f "$DEPLOY_DIR/config.json" ]; then
            $SUDO $INSTALL -m 0640 dist/config.example.json "$DEPLOY_DIR/config.json"
          fi
          $SUDO $INSTALL -m 0644 deploy/supervisor/sp-mcp.conf "$SUPERVISOR_CONF"
          $SUDO $SUPERVISORCTL reread
          $SUDO $SUPERVISORCTL update
          $SUDO $SUPERVISORCTL restart "$SERVICE_NAME"
        '''
      }
    }
  }

  post {
    always {
      archiveArtifacts artifacts: 'dist/**', fingerprint: true, onlyIfSuccessful: true
    }
  }
}
