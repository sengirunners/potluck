package boxport

// The parent retains the TCP socket and owns at most one read worker, one
// write worker and one bounded queued page in each direction. Independent
// workers let reads drain a peer while a backpressured write is pending.
// Close, idle timeout and the absolute lease deadline reap all owned workers.
const guestScript = `
use strict; use warnings; use IO::Socket::INET; use IO::Socket::UNIX; use IO::Select; use Socket qw(SOCK_STREAM AF_INET sockaddr_in inet_aton SOL_SOCKET SO_SNDBUF SO_RCVBUF); use POSIX qw(setsid WNOHANG); use Time::HiRes qw(time);
my ($op,$path,$port,$deadline)=@ARGV;
chdir($ENV{TMPDIR}||"/tmp") or die;
$path=".potluck-port-".$path;
$SIG{ALRM}=sub{die "socket timeout"}; alarm 6;
sub put { my($s,$b)=@_; while(length $b){my$n=syswrite($s,$b);die unless $n;substr($b,0,$n)=""} }
if($op eq 'open'){
 socket(my $tcp,AF_INET,SOCK_STREAM,0) or die;
 # Bound both kernel queues to a page (Linux reports at most twice the
 # requested size). Disable TCP auto-tuning so memory and backpressure do
 # not depend on the host's default multi-megabyte send buffer.
 setsockopt($tcp,SOL_SOCKET,SO_SNDBUF,pack('i',65536)) or die;
 setsockopt($tcp,SOL_SOCKET,SO_RCVBUF,pack('i',65536)) or die;
 for my$option(SO_SNDBUF,SO_RCVBUF){my$size=getsockopt($tcp,SOL_SOCKET,$option);die unless defined($size)&&unpack('i',$size)<=262144}
 connect($tcp,sockaddr_in($port,inet_aton('127.0.0.1'))) or die;
 my $srv=IO::Socket::UNIX->new(Type=>SOCK_STREAM,Local=>$path,Listen=>4) or die;
 chmod 0600,$path;
 my $pid=fork();die unless defined $pid;
 if($pid){print 'O';exit 0}
 close STDIN;close STDOUT;close STDERR;setsid();alarm 0;
 my $last=time;my(%workers,%running,%pending);
 eval {
 while(time<$deadline && time-$last<60){
  for my$p(keys %workers){if(waitpid($p,WNOHANG)==$p){delete$running{$workers{$p}};delete$workers{$p}}}
  for my$code(keys %pending){
   next if $running{$code};my($c,$data)=@{$pending{$code}};
   my$p=fork();die unless defined$p;
   if(!$p){
    close$srv;for my$q(values %pending){close$q->[0] if fileno($q->[0])!=fileno($c)}
    alarm 5;
    my$ok=eval{
     if($code eq 'W'){put($tcp,$data);put($c,'O')}
     else{if(IO::Select->new($tcp)->can_read(0.2)){my$n=sysread($tcp,my$b,65536);die unless defined$n;put($c,($n?'O':'F').$b)}else{put($c,'O')}}
     1;
    };
    close$c;exit($ok?0:1);
   }
   $workers{$p}=$code;$running{$code}=$p;delete$pending{$code};close$c;
  }
  next unless IO::Select->new($srv)->can_read(0.02);
  my$c=$srv->accept() or next;$last=time;alarm 5;
  my$h='';while(length($h)<5){my$n=sysread($c,my$b,5-length($h));die unless$n;$h.=$b}
  my($code,$len)=unpack('aN',$h);die if$len>65536;
  my$data='';while(length($data)<$len){my$n=sysread($c,my$b,$len-length($data));die unless$n;$data.=$b}
  if($code eq 'C'){put($c,'O');close$c;last}
  if(($code ne 'R' && $code ne 'W')||exists$pending{$code}){put($c,'E');close$c}else{$pending{$code}=[$c,$data]}
  alarm 0;
 }
 };
 alarm 0;kill 'KILL',keys %workers if %workers;waitpid($_,0) for keys %workers;
 close($_->[0]) for values %pending;close$tcp;close$srv;unlink$path;exit 0;
}
my$c=IO::Socket::UNIX->new(Type=>SOCK_STREAM,Peer=>$path) or die;
my$data='';if($op eq 'write'){while(1){my$n=sysread(STDIN,my$b,65536);die unless defined$n;last unless$n;$data.=$b;die if length($data)>65536}}
my$code=$op eq 'read'?'R':$op eq 'write'?'W':'C';put($c,pack('aN',$code,length($data)).$data);shutdown($c,1);
my$out='';while(1){my$n=sysread($c,my$b,65537);die unless defined$n;last unless$n;$out.=$b;die if length($out)>65537}print$out;
`
