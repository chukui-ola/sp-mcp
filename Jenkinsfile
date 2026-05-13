pipeline {
  agent any

  options {
    timestamps()
    disableConcurrentBuilds()
    buildDiscarder(logRotator(numToKeepStr: '20'))
  }

  environment {
    CGO_ENABLED = '0'
    GOFLAGS = '-trimpath'
    DEPLOY_DIR = '/var/www/slp/sp-mcp'
    SUPERVISOR_CONF = '/etc/supervisor/conf.d/sp-mcp.conf'
    SERVICE_NAME = 'sp-mcp'
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
          export PATH=$PATH:/var/data/go/1.24.3/go/bin
          go env -w GOPROXY=https://goproxy.cn,direct
        '''
      }
    }

    stage('Go Version') {
      steps {
        sh '''
          export PATH=$PATH:/var/data/go/1.24.3/go/bin
          go version
        '''
      }
    }

    stage('Test') {
      steps {
        sh '''
          export PATH=$PATH:/var/data/go/1.24.3/go/bin
          go test ./...
        '''
      }
    }

    stage('Build') {
      steps {
        sh '''
          export PATH=$PATH:/var/data/go/1.24.3/go/bin
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
          export PATH=$PATH:/var/data/go/1.24.3/go/bin
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
          sudo install -d -m 0755 "$DEPLOY_DIR"
          sudo install -m 0755 dist/sp-mcp "$DEPLOY_DIR/sp-mcp"
          sudo install -m 0644 dist/config.example.json "$DEPLOY_DIR/config.example.json"
          if [ ! -f "$DEPLOY_DIR/config.json" ]; then
            sudo install -m 0640 dist/config.example.json "$DEPLOY_DIR/config.json"
          fi
          sudo install -m 0644 deploy/supervisor/sp-mcp.conf "$SUPERVISOR_CONF"
          sudo supervisorctl reread
          sudo supervisorctl update
          sudo supervisorctl restart "$SERVICE_NAME"
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
