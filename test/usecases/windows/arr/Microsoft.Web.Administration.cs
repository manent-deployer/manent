// Emulation of the part of Microsoft.Web.Administration used by the iis-arr pool of manent
// (use cases): the ARR web farms of applicationHost.config, with the SetState method and the
// state, isHealthy and currentRequests counters of every server. State in arr.json:
// {"farms": {"<farm>": {"<address>": {"enabled", "state", "healthy", "requests"}}}, "history": []}.
// Every SetState is appended to history as "<address>:<state>".
using System;
using System.Collections;
using System.Collections.Generic;
using System.IO;
using System.Text.Json.Nodes;

namespace Microsoft.Web.Administration
{
    internal static class Store
    {
        static string Path => System.IO.Path.Combine(Environment.GetEnvironmentVariable("STATE_DIR") ?? "/srv/win/state", "arr.json");
        public static JsonNode Read() => JsonNode.Parse(File.ReadAllText(Path));
        public static void Write(JsonNode n)
        {
            File.WriteAllText(Path + ".tmp", n.ToJsonString());
            File.Move(Path + ".tmp", Path, true);
        }
    }

    public class ServerManager
    {
        public Configuration GetApplicationHostConfiguration() => new Configuration();
    }

    public class Configuration
    {
        public ConfigurationSection GetSection(string path)
        {
            if (path != "webFarms") throw new ArgumentException("sezione non emulata: " + path);
            return new ConfigurationSection();
        }
    }

    public class ConfigurationElement
    {
        internal readonly Dictionary<string, object> Attrs = new Dictionary<string, object>();
        internal Func<ConfigurationElementCollection> Children;
        internal Func<string, ConfigurationElement> Child;
        internal Dictionary<string, ConfigurationMethod> MethodMap = new Dictionary<string, ConfigurationMethod>();

        public object GetAttributeValue(string name) => Attrs.TryGetValue(name, out var v) ? v : null;
        public void SetAttributeValue(string name, object value) => Attrs[name] = value;
        public ConfigurationElementCollection GetCollection() => Children != null ? Children() : new ConfigurationElementCollection(new List<ConfigurationElement>());
        public ConfigurationElement GetChildElement(string name) => Child != null ? Child(name) : throw new ArgumentException("elemento non emulato: " + name);
        public ConfigurationMethodCollection Methods => new ConfigurationMethodCollection(MethodMap);
    }

    public class ConfigurationSection : ConfigurationElement
    {
        public ConfigurationSection()
        {
            Children = () =>
            {
                var farms = new List<ConfigurationElement>();
                foreach (var f in Store.Read()["farms"].AsObject())
                {
                    var farm = new ConfigurationElement();
                    var name = f.Key;
                    farm.Attrs["name"] = name;
                    farm.Children = () => Servers(name);
                    farms.Add(farm);
                }
                return new ConfigurationElementCollection(farms);
            };
        }

        static ConfigurationElementCollection Servers(string farm)
        {
            var list = new List<ConfigurationElement>();
            foreach (var s in Store.Read()["farms"][farm].AsObject())
            {
                var address = s.Key;
                var server = new ConfigurationElement();
                server.Attrs["address"] = address;
                server.Attrs["enabled"] = (bool)s.Value["enabled"];
                server.Child = name =>
                {
                    if (name != "applicationRequestRouting") throw new ArgumentException("elemento non emulato: " + name);
                    var arr = new ConfigurationElement();
                    arr.Child = c =>
                    {
                        if (c != "counters") throw new ArgumentException("elemento non emulato: " + c);
                        var st = Store.Read()["farms"][farm][address];
                        var counters = new ConfigurationElement();
                        counters.Attrs["state"] = (int)st["state"];
                        counters.Attrs["isHealthy"] = (bool)st["healthy"];
                        counters.Attrs["currentRequests"] = (long)st["requests"];
                        return counters;
                    };
                    arr.MethodMap["SetState"] = new ConfigurationMethod(input =>
                    {
                        var root = Store.Read();
                        var state = Convert.ToInt32(input.GetAttributeValue("newState"));
                        root["farms"][farm][address]["state"] = state;
                        root["history"].AsArray().Add(address + ":" + state);
                        Store.Write(root);
                    });
                    return arr;
                };
                list.Add(server);
            }
            return new ConfigurationElementCollection(list);
        }
    }

    public class ConfigurationElementCollection : IEnumerable
    {
        readonly List<ConfigurationElement> items;
        internal ConfigurationElementCollection(List<ConfigurationElement> items) { this.items = items; }
        public int Count => items.Count;
        public IEnumerator GetEnumerator() => items.GetEnumerator();
    }

    public class ConfigurationMethodCollection
    {
        readonly Dictionary<string, ConfigurationMethod> map;
        internal ConfigurationMethodCollection(Dictionary<string, ConfigurationMethod> map) { this.map = map; }
        public ConfigurationMethod this[string name] => map.TryGetValue(name, out var m) ? m : throw new ArgumentException("metodo non emulato: " + name);
    }

    public class ConfigurationMethod
    {
        readonly Action<ConfigurationElement> run;
        internal ConfigurationMethod(Action<ConfigurationElement> run) { this.run = run; }
        public ConfigurationMethodInstance CreateInstance() => new ConfigurationMethodInstance(run);
    }

    public class ConfigurationMethodInstance
    {
        readonly Action<ConfigurationElement> run;
        internal ConfigurationMethodInstance(Action<ConfigurationElement> run) { this.run = run; }
        public ConfigurationElement Input { get; } = new ConfigurationElement();
        public void Execute() => run(Input);
    }
}
