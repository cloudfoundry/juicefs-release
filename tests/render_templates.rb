require 'erb'
require 'json'
require 'yaml'
require 'ostruct'
require 'minitest/autorun'

ROOT = File.expand_path('..', __dir__)

class RenderContext
  attr_reader :spec

  def initialize(job, properties = {}, links = {}, bootstrap = true)
    job_spec = YAML.load_file(File.join(ROOT, 'jobs', job, 'spec'))
    @properties = job_spec.fetch('properties', {}).transform_values { |value| value['default'] }.merge(properties)
    @links = links
    @spec = OpenStruct.new(id: 'local-vm', bootstrap: bootstrap)
  end

  def p(name)
    @properties.fetch(name)
  end

  def link(name)
    @links.fetch(name)
  end

  def if_link(name)
    yield @links[name] if @links.key?(name)
  end

  def render(job, template)
    ERB.new(File.read(File.join(ROOT, 'jobs', job, 'templates', template))).result(binding)
  end
end

class AcceptanceTemplatesTest < Minitest::Test
  def test_cf_link_and_extra_roots_are_rendered_without_losing_json_escaping
    cc = FakeLink.new({
      'cc.internal_service_hostname' => 'api.service.cf.internal',
      'cc.public_tls.port' => 9024,
      'cc.public_tls.ca_cert' => "cc-root\n"
    })
    properties = {
      'juicefs.cf.password' => "secret\"with\nnewlines",
      'juicefs.cf.additional_ca_certificates' => ['router-root'],
      'juicefs.acceptance.apps_domain' => 'apps.example.com'
    }
    rendered = RenderContext.new('juicefs-acceptance-tests', properties, {'cloud_controller_https_endpoint' => cc}).render('juicefs-acceptance-tests', 'config.json.erb')
    config = JSON.parse(rendered)
    assert_equal 'https://api.service.cf.internal:9024', config['apiURL']
    assert_equal "cc-root\n\nrouter-root", config['caCert']
    assert_equal properties['juicefs.cf.password'], config['password']
    assert_equal 'apps.example.com', config['appsDomain']
    assert_equal false, config['skipSSLValidation']
  end
end

class FakeLink
  attr_reader :instances

  def initialize(properties, id = 'local-vm')
    @properties = properties
    @instances = [OpenStruct.new(id: id)]
  end

  def p(name)
    @properties.fetch(name)
  end
end

class AzureTemplatesTest < Minitest::Test
  def setup
    @metadata = {'type' => 'postgres', 'host' => 'server.postgres.database.azure.com', 'database' => 'juicefs', 'user' => 'identity', 'password' => ''}
    @storage = {'type' => 'wasb', 'bucket' => 'https://container.blob.core.windows.net', 'access_key' => 'account', 'managed_identity' => true, 'managed_identity_client_id' => 'blob-client-id'}
    @proxy = FakeLink.new({'azure_postgres_auth_proxy.port' => 15432})
    @volume = FakeLink.new({'juicefs.metadata' => @metadata, 'juicefs.storage' => @storage, 'juicefs.volume.name' => 'volume'})
  end

  def test_mounter_and_driver_use_local_proxy_and_preserve_blob_identity
    ['juicefs-volume', 'juicefs-volume-driver'].each do |job|
      props = job == 'juicefs-volume' ? {'juicefs.metadata' => @metadata, 'juicefs.storage' => @storage} : {}
      links = {'azure_postgres_proxy' => @proxy}
      links['juicefs_volume'] = @volume if job == 'juicefs-volume-driver'
      rendered = RenderContext.new(job, props, links).render(job, 'config.json.erb')
      cfg = JSON.parse(rendered)
      assert_equal '127.0.0.1', cfg['metadata']['host']
      assert_equal 15432, cfg['metadata']['port']
      assert_equal 'sslmode=disable', cfg['metadata']['options']
      assert_equal 'identity', cfg['metadata']['user']
      assert_equal true, cfg['storage']['managed_identity']
      assert_equal 'blob-client-id', cfg['storage']['managed_identity_client_id']
    end
    assert_equal 'server.postgres.database.azure.com', @metadata['host']
  end

  def test_proxy_link_requires_colocation_and_passwordless_postgres
    props = {'juicefs.metadata' => @metadata, 'juicefs.storage' => @storage}
    remote = FakeLink.new({'azure_postgres_auth_proxy.port' => 5432}, 'remote-vm')
    assert_raises(RuntimeError) { RenderContext.new('juicefs-volume', props, {'azure_postgres_proxy' => remote}).render('juicefs-volume', 'config.json.erb') }
    @metadata['password'] = 'secret'
    assert_raises(RuntimeError) { RenderContext.new('juicefs-volume', props, {'azure_postgres_proxy' => @proxy}).render('juicefs-volume', 'config.json.erb') }
    @metadata['password'] = ''
    @metadata['type'] = 'mysql'
    assert_raises(RuntimeError) { RenderContext.new('juicefs-volume', props, {'azure_postgres_proxy' => @proxy}).render('juicefs-volume', 'config.json.erb') }
  end

  def test_existing_backends_without_proxy_are_preserved
    cfg = JSON.parse(RenderContext.new('juicefs-volume-driver', {}, {'juicefs_volume' => @volume}).render('juicefs-volume-driver', 'config.json.erb'))
    assert_equal @metadata, cfg['metadata']
  end

  def test_proxy_bpm_locks_listener_credentials_and_verified_tls
    props = {'azure_postgres_auth_proxy.host' => 'server.postgres.database.azure.com'}
    render = ->(extra) { YAML.safe_load(RenderContext.new('azure-postgres-auth-proxy', props.merge(extra)).render('azure-postgres-auth-proxy', 'bpm.yml.erb'))['processes'].first }
    process = render.call({})
    assert_includes process['args'], '-listen-addr=127.0.0.1:5432'
    assert_equal 'ManagedIdentityCredential', process['env']['AZURE_TOKEN_CREDENTIALS']
    assert_equal 'system', process['env']['PGSSLROOTCERT']
    assert_equal '/var/vcap/packages/ca-certificates/certs/ca-certificates.crt', process['env']['SSL_CERT_FILE']
    refute process['env'].key?('AZURE_CLIENT_ID')
    custom = render.call({'azure_postgres_auth_proxy.client_id' => 'pg-client-id', 'azure_postgres_auth_proxy.ca_cert' => 'pem'})
    assert_equal 'pg-client-id', custom['env']['AZURE_CLIENT_ID']
    assert_equal '/var/vcap/jobs/azure-postgres-auth-proxy/config/ca.crt', custom['env']['SSL_CERT_FILE']
    assert_raises(RuntimeError) { render.call({'azure_postgres_auth_proxy.port' => 0}) }
    assert_raises(RuntimeError) { render.call({'azure_postgres_auth_proxy.host' => '127.0.0.1'}) }
  end

  def test_initialization_only_runs_in_bootstrap_launcher
    props = {'juicefs.volume.initialize' => true}
    bootstrap = RenderContext.new('juicefs-volume', props)
    assert_includes bootstrap.render('juicefs-volume', 'mount.erb'), 'juicefs-launcher format'
    refute_includes bootstrap.render('juicefs-volume', 'pre-start.erb'), 'juicefs-launcher format'
    refute_includes RenderContext.new('juicefs-volume', props, {}, false).render('juicefs-volume', 'mount.erb'), 'juicefs-launcher format'
    refute_includes RenderContext.new('juicefs-volume').render('juicefs-volume', 'mount.erb'), 'juicefs-launcher format'
  end
end
